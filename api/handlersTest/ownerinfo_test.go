package handlersTest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/fido-device-onboard/go-fdo/sqlite"
	"github.com/kgiusti/go-fdo-server/api/handlers"
	"github.com/kgiusti/go-fdo-server/internal/db"
)

func setupTestServer(t *testing.T, handlerFunc http.HandlerFunc) (*httptest.Server, *sqlite.DB) {

	state, err := sqlite.Open("test.db", "")
	if err != nil {
		t.Fatal(err)
	}

	err = db.InitDb(state)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handlerFunc)

	return server, state
}

func TestOwnerInfoHandler(t *testing.T) {

	cleanup := func() error { return os.Remove("test.db") }
	defer cleanup()

	server, state := setupTestServer(t, handlers.OwnerInfoHandler)
	defer server.Close()
	defer state.Close()

	t.Run("POST OwnerInfo", func(t *testing.T) {
		requestBody := bytes.NewReader([]byte(`["127.0.0.1", "localhost", "8043"]`))

		// Perform the POST request
		response, err := http.Post(server.URL, "text/plain", requestBody)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()

		// Check the response status code
		if response.StatusCode != http.StatusCreated {
			t.Errorf("Status code is %v", response.StatusCode)
		}
	})

	t.Run("GET OwnerInfo", func(t *testing.T) {
		response, _ := http.Get(server.URL)

		if response.StatusCode != http.StatusOK {
			t.Errorf("Status code is %v", response.StatusCode)
		}

		var responseBody db.Data
		err := json.NewDecoder(response.Body).Decode(&responseBody)
		if err != nil {
			t.Errorf("Unable to parse OwnerInfo response %v", err)
		}
		values, _ := responseBody.Value.([]interface{})
		if len(values) != 3 {
			t.Errorf("Wrong OwnerInfo response %v", values)
		}
	})

	t.Run("PUT OwnerInfo", func(t *testing.T) {
		requestBody := bytes.NewReader([]byte(`["127.0.0.1", "localhost", "8080"]`))

		// Create a PUT request
		req, _ := http.NewRequest(http.MethodPut, server.URL, requestBody)
		req.Header.Set("Content-Type", "text/plain")

		// Perform the PUT request
		client := &http.Client{}
		response, err := client.Do(req)
		if err != nil {
			t.Errorf("Unable to connect with Owner endpoint")
		}
		defer response.Body.Close()

		// Check the response status code
		if response.StatusCode != http.StatusOK {
			t.Errorf("Status code is %v", response.StatusCode)
		}
	})

}
