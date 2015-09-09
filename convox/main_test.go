package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"testing"

	"github.com/convox/cli/client"
	"github.com/convox/cli/stdcli"
)

type Stub struct {
	Method   string
	Path     string
	Code     int
	Response interface{}
}

func httpStub(stubs ...Stub) *httptest.Server {
	stubs = append(stubs, Stub{Method: "GET", Path: "/system", Code: 200, Response: client.System{
		Version: "latest",
	}})

	found := false

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, stub := range stubs {
			if stub.Method == r.Method && stub.Path == r.URL.Path {
				data, err := json.Marshal(stub.Response)

				if err != nil {
					http.Error(w, err.Error(), 503)
				}

				w.WriteHeader(stub.Code)
				w.Write(data)

				found = true
				break
			}
		}

		if !found {
			fmt.Printf("unknown request: %+v\n", r)
			http.Error(w, "not found", 404)
		}
	}))

	u, _ := url.Parse(ts.URL)

	dir, _ := ioutil.TempDir("", "convox-test")

	ConfigRoot, _ = ioutil.TempDir("", "convox-test")

	os.Setenv("CONVOX_CONFIG", dir)
	os.Setenv("CONVOX_HOST", u.Host)
	os.Setenv("CONVOX_PASSWORD", "foo")

	return ts
}

func appRun(args []string) (string, string) {
	app := stdcli.New()
	stdcli.Exiter = func(code int) {}
	stdcli.Runner = func(bin string, args ...string) error { return nil }
	stdcli.Querier = func(bin string, args ...string) ([]byte, error) { return []byte{}, nil }
	stdcli.Tagger = func() string { return "1435444444" }
	stdcli.Writer = func(filename string, data []byte, perm os.FileMode) error { return nil }

	// Capture stdout and stderr to strings via Pipes
	oldErr := os.Stderr
	oldOut := os.Stdout

	er, ew, _ := os.Pipe()
	or, ow, _ := os.Pipe()

	os.Stderr = ew
	os.Stdout = ow

	errC := make(chan string)
	// copy the output in a separate goroutine so printing can't block indefinitely
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, er)
		errC <- buf.String()
	}()

	outC := make(chan string)
	// copy the output in a separate goroutine so printing can't block indefinitely
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, or)
		outC <- buf.String()
	}()

	_ = app.Run(args)

	// restore stderr, stdout
	ew.Close()
	os.Stderr = oldErr
	err := <-errC

	ow.Close()
	os.Stdout = oldOut
	out := <-outC

	return out, err
}

func setLoginEnv(ts *httptest.Server) {
	u, _ := url.Parse(ts.URL)

	dir, _ := ioutil.TempDir("", "convox-test")

	ConfigRoot, _ = ioutil.TempDir("", "convox-test")

	os.Setenv("CONVOX_CONFIG", dir)
	os.Setenv("CONVOX_HOST", u.Host)
	os.Setenv("CONVOX_PASSWORD", "foo")
}

func expect(t *testing.T, a interface{}, b interface{}) {
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)

	if !bytes.Equal(aj, bj) {
		t.Errorf("Expected %v (type %v) - Got %v (type %v)", b, reflect.TypeOf(b), a, reflect.TypeOf(a))
	}
}

func refute(t *testing.T, a interface{}, b interface{}) {
	if a == b {
		t.Errorf("Did not expect %v (type %v) - Got %v (type %v)", b, reflect.TypeOf(b), a, reflect.TypeOf(a))
	}
}
