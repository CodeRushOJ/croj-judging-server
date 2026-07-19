package external

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func TestMinIOSourceObjectStoreCreatesWithoutOverwriteAndReadsBounded(t *testing.T) {
	server := newSourceS3Server(t)
	defer server.Close()
	client, err := minio.New(strings.TrimPrefix(server.URL, "http://"), &minio.Options{
		Creds:  credentials.NewStaticV4("test-access", "test-secret-0123456789", ""),
		Secure: false, Region: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewMinIOSourceObjectStore(client, "judge-sources")
	if err != nil {
		t.Fatal(err)
	}
	key := "external/aaaaaaaaaaaaaaaaaaaaaaaaaa/sources/bbbbbbbbbbbbbbbbbbbbbbbbbb.bin"
	first := []byte("authenticated ciphertext")
	if err := store.Create(context.Background(), key, first); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), key, []byte("overwrite attempt")); !errors.Is(err, ErrSourceObjectExists) {
		t.Fatalf("collision error = %v", err)
	}
	loaded, err := store.Get(context.Background(), key, int64(len(first)))
	if err != nil || !bytes.Equal(loaded, first) {
		t.Fatalf("loaded=%q error=%v", loaded, err)
	}
	if _, err := store.Get(context.Background(), key, int64(len(first)-1)); !errors.Is(err, ErrSourceEncryption) {
		t.Fatalf("bounded read error = %v", err)
	}
	if err := store.Delete(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), key, 1024); err == nil {
		t.Fatal("deleted source object remained readable")
	}
}

func TestMinIOSourceObjectStoreRejectsUnsafeConfigurationAndKeys(t *testing.T) {
	if _, err := NewMinIOSourceObjectStore(nil, "judge-sources"); err == nil {
		t.Fatal("nil client accepted")
	}
	server := newSourceS3Server(t)
	defer server.Close()
	client, err := minio.New(strings.TrimPrefix(server.URL, "http://"), &minio.Options{
		Creds:  credentials.NewStaticV4("test-access", "test-secret-0123456789", ""),
		Secure: false, Region: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewMinIOSourceObjectStore(client, "judge-sources")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", "/absolute", "external/../escape", "bundle/not-source"} {
		if err := store.Create(context.Background(), key, []byte("x")); err == nil {
			t.Fatalf("unsafe key %q accepted", key)
		}
	}
}

type sourceS3State struct {
	mutex   sync.Mutex
	objects map[string][]byte
}

func newSourceS3Server(t *testing.T) *httptest.Server {
	t.Helper()
	state := &sourceS3State{objects: make(map[string][]byte)}
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !strings.HasPrefix(request.URL.Path, "/judge-sources/external/") {
			http.NotFound(response, request)
			return
		}
		state.mutex.Lock()
		defer state.mutex.Unlock()
		switch request.Method {
		case http.MethodPut:
			if request.Header.Get("If-None-Match") != "*" {
				t.Errorf("conditional create header = %q", request.Header.Get("If-None-Match"))
			}
			if _, exists := state.objects[request.URL.Path]; exists {
				writeSourceS3Error(response, http.StatusPreconditionFailed, "PreconditionFailed", "object already exists")
				return
			}
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read PUT: %v", err)
				response.WriteHeader(http.StatusInternalServerError)
				return
			}
			if strings.Contains(request.Header.Get("Content-Encoding"), "aws-chunked") ||
				strings.HasPrefix(request.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
				body, err = decodeAWSChunked(body)
				if err != nil {
					t.Errorf("decode aws-chunked PUT: %v", err)
					response.WriteHeader(http.StatusBadRequest)
					return
				}
			}
			state.objects[request.URL.Path] = body
			response.Header().Set("ETag", `"source-etag"`)
			response.WriteHeader(http.StatusOK)
		case http.MethodGet, http.MethodHead:
			body, exists := state.objects[request.URL.Path]
			if !exists {
				writeSourceS3Error(response, http.StatusNotFound, "NoSuchKey", "not found")
				return
			}
			response.Header().Set("Content-Length", strconv.Itoa(len(body)))
			response.Header().Set("ETag", `"source-etag"`)
			response.Header().Set("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
			if request.Method == http.MethodGet {
				_, _ = response.Write(body)
			}
		case http.MethodDelete:
			delete(state.objects, request.URL.Path)
			response.WriteHeader(http.StatusNoContent)
		default:
			response.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
}

func decodeAWSChunked(encoded []byte) ([]byte, error) {
	reader := bufio.NewReader(bytes.NewReader(encoded))
	decoded := make([]byte, 0, len(encoded))
	for {
		header, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		sizeText := strings.SplitN(strings.TrimSpace(header), ";", 2)[0]
		size, err := strconv.ParseInt(sizeText, 16, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("invalid chunk size %q", sizeText)
		}
		if size == 0 {
			return decoded, nil
		}
		start := len(decoded)
		decoded = append(decoded, make([]byte, size)...)
		if _, err := io.ReadFull(reader, decoded[start:]); err != nil {
			return nil, err
		}
		var delimiter [2]byte
		if _, err := io.ReadFull(reader, delimiter[:]); err != nil || delimiter != [2]byte{'\r', '\n'} {
			return nil, fmt.Errorf("invalid chunk delimiter")
		}
	}
}

func writeSourceS3Error(response http.ResponseWriter, status int, code, message string) {
	payload, _ := xml.Marshal(struct {
		XMLName xml.Name `xml:"Error"`
		Code    string   `xml:"Code"`
		Message string   `xml:"Message"`
	}{Code: code, Message: message})
	response.Header().Set("Content-Type", "application/xml")
	response.WriteHeader(status)
	_, _ = response.Write(payload)
}
