// SPDX-FileCopyrightText: (C) 2024 Intel Corporation
// SPDX-License-Identifier: Apache 2.0

package cmd

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"iter"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/fido-device-onboard/go-fdo"
	"github.com/fido-device-onboard/go-fdo-server/api"
	"github.com/fido-device-onboard/go-fdo-server/api/handlers"
	"github.com/fido-device-onboard/go-fdo-server/internal/db"
	"github.com/fido-device-onboard/go-fdo-server/internal/rvinfo"
	"github.com/fido-device-onboard/go-fdo/fsim"
	transport "github.com/fido-device-onboard/go-fdo/http"
	"github.com/fido-device-onboard/go-fdo/protocol"
	"github.com/fido-device-onboard/go-fdo/serviceinfo"
	"github.com/fido-device-onboard/go-fdo/sqlite"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	externalAddress   string
	date              bool
	wgets             []string
	uploads           []string
	uploadDir         string
	downloads         []string
	ownerDeviceCACert string
	ownerPrivateKey   string
	reuseCred         bool
)

// ownerCmd represents the owner command
var ownerCmd = &cobra.Command{
	Use:   "owner http_address",
	Short: "Serve an instance of the owner server",
	PreRunE: func(cmd *cobra.Command, args []string) error {
		// Load configuration first
		if err := ownerCmdLoadConfig(cmd, args); err != nil {
			return err
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		state, err := getState()
		if err != nil {
			return err
		}

		// host, portStr, err := net.SplitHostPort(externalAddress)
		// if err != nil {
		// 	return fmt.Errorf("invalid external addr: %w", err)
		// }

		// portNum, err := strconv.ParseUint(portStr, 10, 16)
		// if err != nil {
		// 	return fmt.Errorf("invalid external port: %w", err)
		// }
		// port := uint16(portNum)

		err = db.InitDb(state)
		if err != nil {
			return err
		}

		return serveOwner(state, insecureTLS)
	},
}

// Server represents the HTTP server
type OwnerServer struct {
	addr    string
	extAddr string
	handler http.Handler
	useTLS  bool
}

// NewServer creates a new Server
func NewOwnerServer(addr string, extAddr string, handler http.Handler, useTLS bool) *OwnerServer {
	return &OwnerServer{addr: addr, extAddr: extAddr, handler: handler, useTLS: useTLS}
}

// Start starts the HTTP server
func (s *OwnerServer) Start() error {
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
			return fmt.Errorf("no TLS cert or key provided")
		}
	}
	return srv.Serve(lis)
}

type OwnerServerState struct {
	DB           *sqlite.DB
	ownerKey     crypto.Signer
	ownerKeyType protocol.KeyType
	chain        []*x509.Certificate
}

func getOwnerServerState(db *sqlite.DB) (*OwnerServerState, error) {
	ownerKey, err := parsePrivateKey(ownerPrivateKey)
	if err != nil {
		return nil, err
	}
	ownerKeyType, err := getPrivateKeyType(ownerKey)
	if err != nil {
		return nil, err
	}
	deviceCA, err := os.ReadFile(ownerDeviceCACert)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(deviceCA)
	if blk == nil {
		return nil, fmt.Errorf("unable to decode device CA")
	}
	parsedDeviceCACert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return nil, err
	}

	return &OwnerServerState{
		DB:           db,
		chain:        []*x509.Certificate{parsedDeviceCACert},
		ownerKey:     ownerKey,
		ownerKeyType: ownerKeyType,
	}, nil
}

func serveOwner(db *sqlite.DB, useTLS bool) error {
	state, err := getOwnerServerState(db)
	if err != nil {
		return err
	}

	to2Server := &fdo.TO2Server{
		Session:         state.DB,
		Vouchers:        state.DB,
		OwnerKeys:       state,
		RvInfo:          func(context.Context, fdo.Voucher) ([][]protocol.RvInstruction, error) { return rvinfo.FetchRvInfo() },
		Modules:         moduleStateMachines{DB: state.DB, states: make(map[string]*moduleStateMachineState)},
		ReuseCredential: func(context.Context, fdo.Voucher) (bool, error) { return reuseCred, nil },
	}

	handler := &transport.Handler{
		Tokens:       state.DB,
		TO2Responder: to2Server,
	}

	// Handle messages
	apiRouter := http.NewServeMux()
	apiRouter.Handle("GET /to0/{guid}", handlers.To0Handler(&handlers.To0HandlerState{
		VoucherState: state.DB,
		KeyState:     state,
		UseTLS:       useTLS,
	}))
	apiRouter.Handle("POST /owner/vouchers", handlers.InsertVoucherHandler([]crypto.PublicKey{state.ownerKey.Public()}))
	apiRouter.HandleFunc("/owner/redirect", handlers.OwnerInfoHandler)
	apiRouter.Handle("POST /owner/resell/{guid}", handlers.ResellHandler(to2Server))
	httpHandler := api.NewHTTPHandler(handler, state.DB).RegisterRoutes(apiRouter)

	// Listen and serve
	server := NewOwnerServer(address, externalAddress, httpHandler, useTLS)

	slog.Debug("Starting server on:", "addr", address)
	return server.Start()
}

func (state *OwnerServerState) OwnerKey(ctx context.Context, keyType protocol.KeyType, rsaBits int) (crypto.Signer, []*x509.Certificate, error) {
	return state.ownerKey, state.chain, nil
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

func init() {
	rootCmd.AddCommand(ownerCmd)

	ownerCmd.Flags().Bool("command-date", false, "Use fdo.command FSIM to have device run \"date --utc\"")
	ownerCmd.Flags().StringArray("command-wget", nil, "Use fdo.wget FSIM for each `url` (flag may be used multiple times)")
	ownerCmd.Flags().StringArray("command-upload", nil, "Use fdo.upload FSIM for each `file` (flag may be used multiple times)")
	ownerCmd.Flags().String("upload-directory", "", "The directory `path` to put file uploads")
	ownerCmd.Flags().StringArray("command-download", nil, "Use fdo.download FSIM for each `file` (flag may be used multiple times)")
	ownerCmd.Flags().Bool("reuse-credentials", false, "Perform the Credential Reuse Protocol in TO2")
	ownerCmd.Flags().String("device-ca-cert", "", "Device CA certificate path")
	ownerCmd.Flags().String("owner-key", "", "Owner private key path")
	ownerCmd.Flags().String("external-address", "", "External `addr`ess devices should connect to (default \"127.0.0.1:${LISTEN_PORT}\")")
	ownerCmd.Flags().String("config", "", "Pathname of the configuration file")
}

// Load configuration from viper
func ownerCmdLoadConfig(cmd *cobra.Command, args []string) error {
	err := viper.BindPFlags(cmd.Flags())
	if err != nil {
		return err
	}

	// If the http_address has been provided on the command line it
	// will take precedence over the content of the configuration
	// file. Yet viper has no visibility of the command line so we
	// force it here:
	if len(args) > 0 {
		viper.Set("address", args[0])
	}

	// Get the config flag directly from the command
	configFilePath, err := cmd.Flags().GetString("config")
	if err != nil {
		return fmt.Errorf("failed to get config flag: %w", err)
	}

	if configFilePath != "" {
		slog.Debug("Loading owner server configuration file", "path", configFilePath)
		viper.SetConfigFile(configFilePath)
		if err := viper.ReadInConfig(); err != nil {
			return fmt.Errorf("configuration file read failed: %w", err)
		}
	}

	// Load root configuration after reading config file
	if err := rootCmdLoadConfig(); err != nil {
		return err
	}

	date = viper.GetBool("command-date")
	wgets = viper.GetStringSlice("command-wget")
	uploads = viper.GetStringSlice("command-upload")
	uploadDir = viper.GetString("upload-directory")
	downloads = viper.GetStringSlice("command-download")
	reuseCred = viper.GetBool("reuse-credentials")
	ownerDeviceCACert = viper.GetString("device-ca-cert")
	ownerPrivateKey = viper.GetString("owner-key")
	externalAddress = viper.GetString("external-address")
	address = viper.GetString("address")

	if address == "" {
		return fmt.Errorf("the owner command requires the 'http_address' argument")
	}

	// Default externalAddress to address if not explicitly provided
	if externalAddress == "" {
		externalAddress = address
	}

	return nil
}
