// SPDX-FileCopyrightText: (C) 2024 Intel Corporation
// SPDX-License-Identifier: Apache 2.0

package cmd

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"log"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/fido-device-onboard/go-fdo"
	"github.com/fido-device-onboard/go-fdo/custom"
	"github.com/fido-device-onboard/go-fdo/fsim"
	transport "github.com/fido-device-onboard/go-fdo/http"
	"github.com/fido-device-onboard/go-fdo/protocol"
	"github.com/fido-device-onboard/go-fdo/serviceinfo"
	"github.com/fido-device-onboard/go-fdo/sqlite"
	"github.com/kgiusti/go-fdo-server/api"
	"github.com/kgiusti/go-fdo-server/internal/db"
	"github.com/kgiusti/go-fdo-server/internal/ownerinfo"
	"github.com/kgiusti/go-fdo-server/internal/rvinfo"
	"github.com/kgiusti/go-fdo-server/internal/to0"
	"github.com/spf13/cobra"
)

var (
	address         string
	insecureTLS     bool
	serverCertPath  string
	serverKeyPath   string
	externalAddress string
	date            bool
	wgets           []string
	uploads         []string
	uploadDir       string
	downloads       []string
	reuseCred       bool
	//
	rvBypass bool
)

// serveCmd represents the serve command
var serveCmd = &cobra.Command{
	Use:   "serve http_address",
	Short: "Serve an instance of the HTTP server for the role",
	Long: `Serve runs the HTTP server for the FDO protocol. It can act as all the three
	main servers in the FDO spec.`,
	Args: func(cmd *cobra.Command, args []string) error {
		if err := cobra.ExactArgs(1)(cmd, args); err != nil {
			return err
		}
		address = args[0]
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		state, err := getState()
		if err != nil {
			return err
		}

		if externalAddress == "" {
			externalAddress = address
		}

		host, portStr, err := net.SplitHostPort(externalAddress)
		if err != nil {
			return fmt.Errorf("invalid external addr: %w", err)
		}

		portNum, err := strconv.ParseUint(portStr, 10, 16)
		if err != nil {
			return fmt.Errorf("invalid external port: %w", err)
		}
		port := uint16(portNum)

		err = db.InitDb(state)
		if err != nil {
			return err
		}

		// set tls for TO0
		to0.SetTo0Tls(insecureTLS)

		// Retrieve RV info from DB
		rvInfo, err := rvinfo.FetchRvInfo()
		if err != nil {
			return err
		}

		if rvInfo != nil {
			rvBypass = rvinfo.HasRVBypass(rvInfo)
		} else {
			rvBypass = false
		}

		// CreateRvInfo initializes new RV info if not found in DB
		if rvInfo == nil {
			rvInfo, err = rvinfo.CreateRvInfo(insecureTLS, host, port)
			if err != nil {
				return err
			}
		}

		// CreateRvTO2Addr initializes new owner info and stores it with default values if not found in DB
		err = ownerinfo.CreateRvTO2Addr(host, port, insecureTLS)
		if err != nil {
			return fmt.Errorf("failed to create and store rvTO2Addrs: %v", err)
		}

		return serveHTTP(rvInfo, state, insecureTLS)
	},
}

// Server represents the HTTP server
type Server struct {
	addr    string
	extAddr string
	handler http.Handler
	useTLS  bool
	state   *sqlite.DB
}

// NewServer creates a new Server
func NewServer(addr string, extAddr string, handler http.Handler, useTLS bool, state *sqlite.DB) *Server {
	return &Server{addr: addr, extAddr: extAddr, handler: handler, useTLS: useTLS, state: state}
}

// Start starts the HTTP server
func (s *Server) Start() error {
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 3 * time.Second,
	}

	// Channel to listen for interrupt or terminate signals
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	// Goroutine to listen for signals and gracefully shut down the server
	go func() {
		<-stop
		slog.Debug("Shutting down server...")

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := srv.Shutdown(ctx); err != nil {
			slog.Debug("Server forced to shutdown:", "err", err)
		}
	}()

	// Listen and serve
	lis, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	defer func() { _ = lis.Close() }()
	slog.Info("Listening", "local", lis.Addr().String(), "external", s.extAddr)

	if s.useTLS {

		preferredCipherSuites := []uint16{
			tls.TLS_AES_256_GCM_SHA384,                  // TLS v1.3
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,   // TLS v1.2
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384, // TLS v1.2
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, // TLS v1.2
		}

		if serverCertPath != "" && serverKeyPath != "" {
			srv.TLSConfig = &tls.Config{
				MinVersion:   tls.VersionTLS12,
				CipherSuites: preferredCipherSuites,
			}
			return srv.ServeTLS(lis, serverCertPath, serverKeyPath)
		} else {
			cert, err := tlsCert(s.state.DB())
			if err != nil {
				return err
			}
			srv.TLSConfig = &tls.Config{
				MinVersion:   tls.VersionTLS12,
				Certificates: []tls.Certificate{*cert},
				CipherSuites: preferredCipherSuites,
			}
			return srv.ServeTLS(lis, "", "")

		}
	}
	return srv.Serve(lis)
}

type ServerState struct {
	RvInfo [][]protocol.RvInstruction
	DB     *sqlite.DB
}

func serveHTTP(rvInfo [][]protocol.RvInstruction, db *sqlite.DB, useTLS bool) error {
	state := &ServerState{
		RvInfo: rvInfo,
		DB:     db,
	}
	// Create FDO responder
	handler, err := newHandler(state)
	if err != nil {
		return err
	}

	// Handle messages
	httpHandler := api.NewHTTPHandler(handler, &state.RvInfo, state.DB).RegisterRoutes()
	// Listen and serve
	server := NewServer(address, externalAddress, httpHandler, useTLS, state.DB)

	slog.Debug("Starting server on:", "addr", address)
	return server.Start()
}

//nolint:gocyclo
func newHandler(state *ServerState) (*transport.Handler, error) {
	aio := fdo.AllInOne{
		DIAndOwner:         state.DB,
		RendezvousAndOwner: withOwnerAddrs{state.DB, state.RvInfo},
	}
	autoExtend := aio.Extend
	// Generate manufacturing component keys
	rsa2048MfgKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	rsa3072MfgKey, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return nil, err
	}
	ec256MfgKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	ec384MfgKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return nil, err
	}
	generateCA := func(key crypto.Signer) ([]*x509.Certificate, error) {
		template := &x509.Certificate{
			SerialNumber:          big.NewInt(1),
			Subject:               pkix.Name{CommonName: "Test CA"},
			NotBefore:             time.Now(),
			NotAfter:              time.Now().Add(30 * 365 * 24 * time.Hour),
			BasicConstraintsValid: true,
			IsCA:                  true,
		}
		der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
		if err != nil {
			return nil, err
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, err
		}
		return []*x509.Certificate{cert}, nil
	}
	rsa2048Chain, err := generateCA(rsa2048MfgKey)
	if err != nil {
		return nil, err
	}
	rsa3072Chain, err := generateCA(rsa3072MfgKey)
	if err != nil {
		return nil, err
	}
	ec256Chain, err := generateCA(ec256MfgKey)
	if err != nil {
		return nil, err
	}
	ec384Chain, err := generateCA(ec384MfgKey)
	if err != nil {
		return nil, err
	}
	if err := state.DB.AddManufacturerKey(protocol.Rsa2048RestrKeyType, rsa2048MfgKey, rsa2048Chain); err != nil {
		return nil, err
	}
	if err := state.DB.AddManufacturerKey(protocol.RsaPkcsKeyType, rsa3072MfgKey, rsa3072Chain); err != nil {
		return nil, err
	}
	if err := state.DB.AddManufacturerKey(protocol.RsaPssKeyType, rsa3072MfgKey, rsa3072Chain); err != nil {
		return nil, err
	}
	if err := state.DB.AddManufacturerKey(protocol.Secp256r1KeyType, ec256MfgKey, ec256Chain); err != nil {
		return nil, err
	}
	if err := state.DB.AddManufacturerKey(protocol.Secp384r1KeyType, ec384MfgKey, ec384Chain); err != nil {
		return nil, err
	}

	// Generate owner keys
	rsa2048OwnerKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	rsa3072OwnerKey, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return nil, err
	}
	ec256OwnerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	ec384OwnerKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := state.DB.AddOwnerKey(protocol.Rsa2048RestrKeyType, rsa2048OwnerKey, nil); err != nil {
		return nil, err
	}
	if err := state.DB.AddOwnerKey(protocol.RsaPkcsKeyType, rsa3072OwnerKey, nil); err != nil {
		return nil, err
	}
	if err := state.DB.AddOwnerKey(protocol.RsaPssKeyType, rsa3072OwnerKey, nil); err != nil {
		return nil, err
	}
	if err := state.DB.AddOwnerKey(protocol.Secp256r1KeyType, ec256OwnerKey, nil); err != nil {
		return nil, err
	}
	if err := state.DB.AddOwnerKey(protocol.Secp384r1KeyType, ec384OwnerKey, nil); err != nil {
		return nil, err
	}

	// Auto-register RV blob so that TO1 can be tested unless a TO0 address is
	// given or RV bypass is set
	var autoTO0 func(context.Context, fdo.Voucher) error
	if !rvBypass {
		autoTO0 = aio.RegisterOwnerAddr
	}

	return &transport.Handler{
		Tokens: state.DB,
		DIResponder: &fdo.DIServer[custom.DeviceMfgInfo]{
			Session:               state.DB,
			Vouchers:              state.DB,
			SignDeviceCertificate: custom.SignDeviceCertificate(state.DB),
			DeviceInfo: func(_ context.Context, info *custom.DeviceMfgInfo, _ []*x509.Certificate) (string, protocol.KeyType, protocol.KeyEncoding, error) {
				return info.DeviceInfo, info.KeyType, info.KeyEncoding, nil
			},
			BeforeVoucherPersist: autoExtend,
			AfterVoucherPersist:  autoTO0,
			RvInfo:               func(context.Context, *fdo.Voucher) ([][]protocol.RvInstruction, error) { return state.RvInfo, nil },
		},
		TO0Responder: &fdo.TO0Server{
			Session: state.DB,
			RVBlobs: state.DB,
		},
		TO1Responder: &fdo.TO1Server{
			Session: state.DB,
			RVBlobs: state.DB,
		},
		TO2Responder: &fdo.TO2Server{
			Session:         state.DB,
			Vouchers:        state.DB,
			OwnerKeys:       state.DB,
			RvInfo:          func(context.Context, fdo.Voucher) ([][]protocol.RvInstruction, error) { return state.RvInfo, nil },
			Modules:         moduleStateMachines{DB: state.DB, states: make(map[string]*moduleStateMachineState)},
			ReuseCredential: func(context.Context, fdo.Voucher) (bool, error) { return reuseCred, nil }},
	}, nil
}

type moduleStateMachines struct {
	DB *sqlite.DB
	// current module state machine state for all sessions (indexed by token)
	states map[string]*moduleStateMachineState
}

type moduleStateMachineState struct {
	Name string
	Impl serviceinfo.OwnerModule
	Next func() (string, serviceinfo.OwnerModule, bool)
	Stop func()
}

func (s moduleStateMachines) Module(ctx context.Context) (string, serviceinfo.OwnerModule, error) {
	token, ok := s.DB.TokenFromContext(ctx)
	if !ok {
		return "", nil, fmt.Errorf("invalid context: no token")
	}
	module, ok := s.states[token]
	if !ok {
		return "", nil, fmt.Errorf("NextModule not called")
	}
	return module.Name, module.Impl, nil
}

func (s moduleStateMachines) NextModule(ctx context.Context) (bool, error) {
	token, ok := s.DB.TokenFromContext(ctx)
	if !ok {
		return false, fmt.Errorf("invalid context: no token")
	}
	module, ok := s.states[token]
	if !ok {
		// Create a new module state machine
		_, modules, _, err := s.DB.Devmod(ctx)
		if err != nil {
			return false, fmt.Errorf("error getting devmod: %w", err)
		}
		next, stop := iter.Pull2(ownerModules(modules))
		module = &moduleStateMachineState{
			Next: next,
			Stop: stop,
		}
		s.states[token] = module
	}

	var valid bool
	module.Name, module.Impl, valid = module.Next()
	return valid, nil
}

func (s moduleStateMachines) CleanupModules(ctx context.Context) {
	token, ok := s.DB.TokenFromContext(ctx)
	if !ok {
		return
	}
	module, ok := s.states[token]
	if !ok {
		return
	}
	module.Stop()
	delete(s.states, token)
}

func ownerModules(modules []string) iter.Seq2[string, serviceinfo.OwnerModule] { //nolint:gocyclo
	return func(yield func(string, serviceinfo.OwnerModule) bool) {
		if slices.Contains(modules, "fdo.download") {
			for _, name := range downloads {
				f, err := os.Open(filepath.Clean(name))
				if err != nil {
					log.Fatalf("error opening %q for download FSIM: %v", name, err)
				}
				defer func() { _ = f.Close() }()

				if !yield("fdo.download", &fsim.DownloadContents[*os.File]{
					Name:         name,
					Contents:     f,
					MustDownload: true,
				}) {
					return
				}
			}
		}

		if slices.Contains(modules, "fdo.upload") {
			for _, name := range uploads {
				if !yield("fdo.upload", &fsim.UploadRequest{
					Dir:  uploadDir,
					Name: name,
				}) {
					return
				}
			}
		}

		if slices.Contains(modules, "fdo.wget") {
			for _, urlString := range wgets {
				url, err := url.Parse(urlString)
				if err != nil || url.Path == "" {
					continue
				}
				if !yield("fdo.wget", &fsim.WgetCommand{
					Name: path.Base(url.Path),
					URL:  url,
				}) {
					return
				}
			}
		}

		if date && slices.Contains(modules, "fdo.command") {
			if !yield("fdo.command", &fsim.RunCommand{
				Command: "date",
				Args:    []string{"--utc"},
				Stdout:  os.Stdout,
				Stderr:  os.Stderr,
			}) {
				return
			}
		}
	}
}

func tlsCert(db *sql.DB) (*tls.Certificate, error) {
	// Ensure that the https table exists
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS https
		( cert BLOB NOT NULL
		, key BLOB NOT NULL
		)`); err != nil {
		return nil, err
	}

	// Load a TLS cert and key from the database
	row := db.QueryRow("SELECT cert, key FROM https LIMIT 1")
	var certDer, keyDer []byte
	if err := row.Scan(&certDer, &keyDer); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if len(keyDer) > 0 {
		key, err := x509.ParsePKCS8PrivateKey(keyDer)
		if err != nil {
			return nil, fmt.Errorf("bad HTTPS key stored: %w", err)
		}
		return &tls.Certificate{
			Certificate: [][]byte{certDer},
			PrivateKey:  key,
		}, nil
	}

	// Generate a new self-signed TLS CA
	tlsKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return nil, err
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test CA"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(30 * 365 * 24 * time.Hour),
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, tlsKey.Public(), tlsKey)
	if err != nil {
		return nil, err
	}
	tlsCA, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, err
	}

	// Store TLS cert and key to the database
	keyDER, err := x509.MarshalPKCS8PrivateKey(tlsKey)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec("INSERT INTO https (cert, key) VALUES (?, ?)", caDER, keyDER); err != nil {
		return nil, err
	}

	// Use CA to serve TLS
	return &tls.Certificate{
		Certificate: [][]byte{tlsCA.Raw},
		PrivateKey:  tlsKey,
	}, nil
}

type withOwnerAddrs struct {
	*sqlite.DB
	RVInfo [][]protocol.RvInstruction
}

func (s withOwnerAddrs) OwnerAddrs(context.Context, fdo.Voucher) ([]protocol.RvTO2Addr, time.Duration, error) {
	var autoTO0Addrs []protocol.RvTO2Addr
	for _, directive := range protocol.ParseDeviceRvInfo(s.RVInfo) {
		if directive.Bypass {
			continue
		}

		for _, url := range directive.URLs {
			to1Host := url.Hostname()
			to1Port, err := strconv.ParseUint(url.Port(), 10, 16)
			if err != nil {
				return nil, 0, fmt.Errorf("error parsing TO1 port to use for TO2: %w", err)
			}
			proto := protocol.HTTPTransport
			if insecureTLS {
				proto = protocol.HTTPSTransport
			}
			autoTO0Addrs = append(autoTO0Addrs, protocol.RvTO2Addr{
				DNSAddress:        &to1Host,
				Port:              uint16(to1Port),
				TransportProtocol: proto,
			})
		}
	}
	return autoTO0Addrs, 0, nil
}

func init() {
	rootCmd.AddCommand(serveCmd)
	// TODO(runcom): bind to viper
	serveCmd.Flags().BoolVar(&insecureTLS, "insecure-tls", false, "Listen with a self-signed TLS certificate")
	serveCmd.Flags().StringVar(&serverCertPath, "server-cert-path", "", "Path to server certificate")
	serveCmd.Flags().StringVar(&serverKeyPath, "server-key-path", "", "Path to server private key")
	serveCmd.Flags().StringVar(&externalAddress, "external-address", "", "External `addr`ess devices should connect to (default \"127.0.0.1:${LISTEN_PORT}\")")
	serveCmd.Flags().BoolVar(&date, "command-date", false, "Use fdo.command FSIM to have device run \"date --utc\"")
	serveCmd.Flags().StringArrayVar(&wgets, "command-wget", nil, "Use fdo.wget FSIM for each `url` (flag may be used multiple times)")
	serveCmd.Flags().StringArrayVar(&uploads, "command-upload", nil, "Use fdo.upload FSIM for each `file` (flag may be used multiple times)")
	serveCmd.Flags().StringVar(&uploadDir, "upload-directory", "", "The directory `path` to put file uploads")
	serveCmd.Flags().StringArrayVar(&downloads, "command-download", nil, "Use fdo.download FSIM for each `file` (flag may be used multiple times)")
	serveCmd.Flags().BoolVar(&reuseCred, "reuse-credentials", false, "Perform the Credential Reuse Protocol in TO2")
}
