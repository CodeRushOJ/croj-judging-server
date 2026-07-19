package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/bundle"
	"github.com/CodeRushOJ/croj-judging-server/internal/callback"
	"github.com/CodeRushOJ/croj-judging-server/internal/discovery"
	"github.com/CodeRushOJ/croj-judging-server/internal/external"
	judgesandbox "github.com/CodeRushOJ/croj-judging-server/internal/sandbox"
	"github.com/CodeRushOJ/croj-judging-server/internal/scheduler"
	"github.com/CodeRushOJ/croj-judging-server/internal/service"
	"github.com/CodeRushOJ/croj-judging-server/pkg/model"
	sandboxpb "github.com/CodeRushOJ/croj-judging-server/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gorm.io/datatypes"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const integrationServiceToken = "0123456789abcdef0123456789abcdef"

func TestExternalCanonicalLanguageRegistryExecutesAgainstRealGRPCFakeSandbox(t *testing.T) {
	language, ok := external.ResolveLanguage("cpp")
	if !ok || language.SandboxID != "cpp" {
		t.Fatalf("canonical language = %+v available=%v", language, ok)
	}
	address, stop := startGRPCSandbox(t, func(_ context.Context, request *sandboxpb.ExecuteRequest) (*sandboxpb.ExecuteResponse, error) {
		if request.Language != language.SandboxID || request.SourceCode != "int main(){}" {
			t.Fatalf("Sandbox received drifted canonical request: %+v", request)
		}
		return &sandboxpb.ExecuteResponse{Status: "Accepted", Stdout: request.ExpectedOutput, TimeUsed: 7, MemoryUsed: 256}, nil
	})
	defer stop()
	api := newEndpointSliceAPI(t)
	api.SetEndpoint(t, address)
	discoverer, err := discovery.NewKubernetesDiscovery("coderushoj", "croj-sandbox", "grpc", writeKubeconfig(t, api.URL()))
	if err != nil {
		t.Fatal(err)
	}
	selector := scheduler.New(discoverer)
	if err := selector.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	client := judgesandbox.NewClientWithCache(2*time.Second, 2, time.Minute)
	defer client.Close()
	metadata, provider := immutableBundle(t, "hidden input", "hidden output\n")
	artifact, err := provider.OpenMetadata(context.Background(), bundle.Metadata{
		ObjectKey: metadata.ObjectKey, SHA256: metadata.SHA256, SizeBytes: metadata.SizeBytes,
	}, metadata.ManifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	defer artifact.Close()
	result, err := service.NewBatchBundlePipeline(selector, client, 1).ExecuteCanonical(context.Background(), service.CanonicalExecutionRequest{
		Language: language.SandboxID, SourceCode: "int main(){}", StopOnFailure: true,
	}, artifact)
	if err != nil || result.Status != callback.StatusAccepted || result.TimeUsedMillis != 7 {
		t.Fatalf("canonical result=%+v error=%v", result, err)
	}
}

func TestImmutableBundleUsesEndpointSliceFailoverGRPCAndRedactedCallback(t *testing.T) {
	const (
		hiddenInput      = "HIDDEN_INPUT_8d9f2b"
		hiddenOutput     = "HIDDEN_OUTPUT_5a13cc\n"
		contestantSrc    = "SOURCE_SENTINEL_706de1"
		compileSourceSrc = "COMPILE_SOURCE_SENTINEL_950fc4"
	)

	var overloadedCalls atomic.Int32
	var healthyCalls atomic.Int32
	var sandboxScheduler *scheduler.Scheduler
	api := newEndpointSliceAPI(t)

	healthyAddress, stopHealthy := startGRPCSandbox(t, func(_ context.Context, request *sandboxpb.ExecuteRequest) (*sandboxpb.ExecuteResponse, error) {
		healthyCalls.Add(1)
		if request.Stdin != hiddenInput || request.ExpectedOutput != hiddenOutput {
			t.Errorf("sandbox request did not preserve the immutable case contract: %+v", request)
		}
		switch request.SourceCode {
		case contestantSrc:
			return &sandboxpb.ExecuteResponse{Status: "Accepted", Stdout: request.ExpectedOutput, TimeUsed: 9, MemoryUsed: 512}, nil
		case compileSourceSrc:
			return &sandboxpb.ExecuteResponse{
				Status:       "Compile Error",
				CompileError: request.SourceCode + " " + request.Stdin + " " + request.ExpectedOutput,
				Stderr:       request.ExpectedOutput,
				Error:        request.Stdin,
			}, nil
		default:
			t.Errorf("unexpected source code %q", request.SourceCode)
			return nil, status.Error(codes.Internal, "unexpected test request")
		}
	})
	defer stopHealthy()

	overloadedAddress, stopOverloaded := startGRPCSandbox(t, func(_ context.Context, _ *sandboxpb.ExecuteRequest) (*sandboxpb.ExecuteResponse, error) {
		overloadedCalls.Add(1)
		api.SetEndpoint(t, healthyAddress)
		if err := sandboxScheduler.Refresh(context.Background()); err != nil {
			t.Errorf("refresh EndpointSlice after churn: %v", err)
		}
		return nil, status.Error(codes.ResourceExhausted, "capacity exhausted")
	})
	defer stopOverloaded()
	api.SetEndpoint(t, overloadedAddress)

	kubeconfig := writeKubeconfig(t, api.URL())
	discoverer, err := discovery.NewKubernetesDiscovery("coderushoj", "croj-sandbox", "grpc", kubeconfig)
	if err != nil {
		t.Fatalf("NewKubernetesDiscovery: %v", err)
	}
	sandboxScheduler = scheduler.New(discoverer)
	if err := sandboxScheduler.Refresh(context.Background()); err != nil {
		t.Fatalf("initial EndpointSlice refresh: %v", err)
	}

	sandboxClient := judgesandbox.NewClientWithCache(2*time.Second, 4, time.Minute)
	defer sandboxClient.Close()
	metadata, provider := immutableBundle(t, hiddenInput, hiddenOutput)
	executor := service.NewHiddenTestExecutor(provider, service.NewBatchBundlePipeline(sandboxScheduler, sandboxClient, 2))

	var callbackResults []callback.Result
	callbackServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/internal/v1/judge-results" || request.Header.Get("X-CROJ-Service-Token") != integrationServiceToken {
			t.Errorf("invalid authenticated callback request: path=%q", request.URL.Path)
		}
		var result callback.Result
		if err := json.NewDecoder(request.Body).Decode(&result); err != nil {
			t.Errorf("decode callback: %v", err)
		}
		callbackResults = append(callbackResults, result)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"code":20000,"success":true,"data":{"disposition":"APPLIED"}}`))
	}))
	defer callbackServer.Close()
	resultClient, err := callback.NewClient(callbackServer.URL+"/api", integrationServiceToken, time.Second, callbackServer.Client())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	store := immutableStore(metadata, contestantSrc)
	judge := service.NewJudgeService(store, executor, resultClient, service.NewTaskRegistry(8, time.Hour))
	if err := judge.ProcessEvent(context.Background(), submissionEvent()); err != nil {
		t.Fatalf("ProcessEvent: %v", err)
	}
	if overloadedCalls.Load() != 1 || healthyCalls.Load() != 1 {
		t.Fatalf("sandbox calls overload=%d healthy=%d, want one failover call each", overloadedCalls.Load(), healthyCalls.Load())
	}
	if len(callbackResults) != 1 || callbackResults[0].Status != callback.StatusAccepted || callbackResults[0].TimeUsedMillis != 9 || callbackResults[0].MemoryUsedKB != 512 {
		t.Fatalf("callback results = %+v", callbackResults)
	}
	serialized, _ := json.Marshal(callbackResults[0])
	for _, secret := range []string{hiddenInput, strings.TrimSpace(hiddenOutput), contestantSrc} {
		if bytes.Contains(serialized, []byte(secret)) {
			t.Fatalf("callback leaked hidden or contestant payload %q: %s", secret, serialized)
		}
	}

	store.submission.Code = compileSourceSrc
	compileEvent := submissionEvent()
	compileEvent.EventID = "ba4f1734-74dd-43ed-9fdb-bd9a178d8356"
	if err := judge.ProcessEvent(context.Background(), compileEvent); err != nil {
		t.Fatalf("ProcessEvent compile error: %v", err)
	}
	if len(callbackResults) != 2 || callbackResults[1].Status != callback.StatusCompileError || callbackResults[1].CompileError != "compilation failed; diagnostics redacted" {
		t.Fatalf("compile callback results = %+v", callbackResults)
	}
	compileSerialized, _ := json.Marshal(callbackResults[1])
	for _, secret := range []string{hiddenInput, strings.TrimSpace(hiddenOutput), compileSourceSrc} {
		if bytes.Contains(compileSerialized, []byte(secret)) {
			t.Fatalf("compile callback leaked hidden or contestant payload %q: %s", secret, compileSerialized)
		}
	}
}

func TestBundleDigestMismatchFailsClosedBeforeSandboxExecution(t *testing.T) {
	metadata, provider := immutableBundle(t, "hidden-in", "hidden-out")
	metadata.SHA256 = strings.Repeat("0", sha256.Size*2)
	recorder := &resultRecorder{}
	executor := service.NewHiddenTestExecutor(provider, service.NewBatchBundlePipeline(nil, nil, 1))
	judge := service.NewJudgeService(immutableStore(metadata, "source"), executor, recorder, service.NewTaskRegistry(8, time.Hour))
	if err := judge.ProcessEvent(context.Background(), submissionEvent()); err != nil {
		t.Fatalf("ProcessEvent: %v", err)
	}
	if recorder.calls != 1 || recorder.result.Status != callback.StatusSystemError {
		t.Fatalf("callback = %+v calls=%d", recorder.result, recorder.calls)
	}
	if strings.Contains(recorder.result.Stderr, "SHA-256") || strings.Contains(recorder.result.Stderr, "hidden") {
		t.Fatalf("fail-closed callback exposed bundle internals: %+v", recorder.result)
	}
}

type endpointSliceAPI struct {
	server *httptest.Server
	mu     sync.RWMutex
	host   string
	port   int32
}

func newEndpointSliceAPI(t *testing.T) *endpointSliceAPI {
	t.Helper()
	api := &endpointSliceAPI{}
	api.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/apis/discovery.k8s.io/v1/namespaces/coderushoj/endpointslices" {
			http.NotFound(writer, request)
			return
		}
		if selector := request.URL.Query().Get("labelSelector"); selector != discoveryv1.LabelServiceName+"=croj-sandbox" {
			t.Errorf("EndpointSlice labelSelector = %q", selector)
		}
		api.mu.RLock()
		host, port := api.host, api.port
		api.mu.RUnlock()
		ready := true
		portName := "grpc"
		list := discoveryv1.EndpointSliceList{
			TypeMeta: metav1.TypeMeta{APIVersion: "discovery.k8s.io/v1", Kind: "EndpointSliceList"},
			Items: []discoveryv1.EndpointSlice{{
				TypeMeta:    metav1.TypeMeta{APIVersion: "discovery.k8s.io/v1", Kind: "EndpointSlice"},
				ObjectMeta:  metav1.ObjectMeta{Name: "croj-sandbox-test", Labels: map[string]string{discoveryv1.LabelServiceName: "croj-sandbox"}},
				AddressType: discoveryv1.AddressTypeIPv4,
				Ports:       []discoveryv1.EndpointPort{{Name: &portName, Protocol: protocolPointer(corev1.ProtocolTCP), Port: &port}},
				Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{host}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}}},
			}},
		}
		writer.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(writer).Encode(list); err != nil {
			t.Errorf("encode EndpointSliceList: %v", err)
		}
	}))
	t.Cleanup(api.server.Close)
	return api
}

func protocolPointer(value corev1.Protocol) *corev1.Protocol { return &value }

func (api *endpointSliceAPI) URL() string { return api.server.URL }

func (api *endpointSliceAPI) SetEndpoint(t *testing.T, address string) {
	t.Helper()
	host, rawPort, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("split sandbox address %q: %v", address, err)
	}
	port, err := strconv.ParseInt(rawPort, 10, 32)
	if err != nil {
		t.Fatalf("parse sandbox port: %v", err)
	}
	api.mu.Lock()
	api.host, api.port = host, int32(port)
	api.mu.Unlock()
}

type grpcSandbox struct {
	sandboxpb.UnimplementedSandboxServiceServer
	execute func(context.Context, *sandboxpb.ExecuteRequest) (*sandboxpb.ExecuteResponse, error)
}

func (server *grpcSandbox) Execute(ctx context.Context, request *sandboxpb.ExecuteRequest) (*sandboxpb.ExecuteResponse, error) {
	return server.execute(ctx, request)
}

func (server *grpcSandbox) ExecuteBatchV1(request *sandboxpb.ExecuteBatchV1Request, stream grpc.ServerStreamingServer[sandboxpb.ExecuteBatchV1Event]) error {
	for _, testCase := range request.Cases {
		response, err := server.execute(stream.Context(), &sandboxpb.ExecuteRequest{
			Language:       request.Language,
			SourceCode:     request.SourceCode,
			Stdin:          testCase.Stdin,
			Timeout:        request.Timeout,
			MemoryLimit:    request.MemoryLimit,
			ExpectedOutput: testCase.ExpectedOutput,
		})
		if err != nil {
			return err
		}
		if response.Status == "Compile Error" {
			return stream.Send(&sandboxpb.ExecuteBatchV1Event{Kind: sandboxpb.ExecuteBatchV1Event_COMPILE_ERROR, Result: response})
		}
		if err := stream.Send(&sandboxpb.ExecuteBatchV1Event{
			Kind:   sandboxpb.ExecuteBatchV1Event_CASE_RESULT,
			CaseId: testCase.CaseId,
			Result: response,
		}); err != nil {
			return err
		}
		if request.StopOnFailure && response.Status != "Accepted" {
			break
		}
	}
	return stream.Send(&sandboxpb.ExecuteBatchV1Event{Kind: sandboxpb.ExecuteBatchV1Event_COMPLETED})
}

func startGRPCSandbox(t *testing.T, execute func(context.Context, *sandboxpb.ExecuteRequest) (*sandboxpb.ExecuteResponse, error)) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer()
	sandboxpb.RegisterSandboxServiceServer(server, &grpcSandbox{execute: execute})
	go func() { _ = server.Serve(listener) }()
	return listener.Addr().String(), func() {
		server.Stop()
		_ = listener.Close()
	}
}

func writeKubeconfig(t *testing.T, serverURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	content := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: %s
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user: {}
`, serverURL)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

type byteStore struct{ data []byte }

func (store byteStore) Open(context.Context, string) (bundle.Object, error) {
	return bundle.Object{Body: io.NopCloser(bytes.NewReader(store.data)), Size: int64(len(store.data))}, nil
}

func immutableBundle(t *testing.T, input, output string) (*model.TestBundle, *bundle.Provider) {
	t.Helper()
	manifest := bundle.Manifest{
		SchemaVersion: 1,
		JudgeMode:     bundle.JudgeModeACM,
		Checker:       bundle.CheckerExact,
		Limits:        bundle.Limits{TimeLimitMillis: 1000, MemoryLimitMiB: 64},
		Cases:         []bundle.Case{{ID: "case-01", Input: "cases/01.in", Output: "cases/01.out", Weight: 1}},
	}
	var archive bytes.Buffer
	manifestJSON, err := bundle.WriteDeterministicArchive(&archive, manifest, map[string][]byte{
		"cases/01.in":  []byte(input),
		"cases/01.out": []byte(output),
	})
	if err != nil {
		t.Fatalf("WriteDeterministicArchive: %v", err)
	}
	digest := sha256.Sum256(archive.Bytes())
	cache, err := bundle.NewCache(t.TempDir(), 1<<20, 1<<20, time.Hour, byteStore{data: archive.Bytes()})
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	return &model.TestBundle{
		ProblemVersionID: 7,
		ObjectKey:        "problems/42/versions/7/tests.zip",
		SHA256:           hex.EncodeToString(digest[:]),
		SizeBytes:        int64(archive.Len()),
		ManifestJSON:     datatypes.JSON(manifestJSON),
	}, bundle.NewProvider(cache, bundle.DefaultArchiveLimits())
}

type submissionStore struct {
	submission *model.Task
	version    *model.ProblemVersion
	bundle     *model.TestBundle
}

func immutableStore(testBundle *model.TestBundle, source string) *submissionStore {
	versionID := int64(7)
	return &submissionStore{
		submission: &model.Task{ID: 99, ProblemID: 42, ProblemVersionID: &versionID, UserID: 7, Language: "go", Code: source, Status: model.StatusPending},
		version: &model.ProblemVersion{
			ID: 7, ProblemID: 42, State: "PUBLISHED",
			LimitsJSON:      datatypes.JSON([]byte(`{"timeLimit":1000,"memoryLimit":64,"totalScore":100}`)),
			JudgeConfigJSON: datatypes.JSON([]byte(`{"specialJudge":false,"specialJudgeCode":null,"specialJudgeLanguage":null,"judgeMode":0}`)),
		},
		bundle: testBundle,
	}
}

func (store *submissionStore) GetSubmissionByID(int64) (*model.Task, error) {
	return store.submission, nil
}
func (store *submissionStore) GetProblemVersionByID(int64) (*model.ProblemVersion, error) {
	return store.version, nil
}
func (store *submissionStore) GetTestBundleByProblemVersionID(int64) (*model.TestBundle, error) {
	return store.bundle, nil
}

func submissionEvent() model.SubmissionRequested {
	return model.SubmissionRequested{SchemaVersion: 1, EventID: "50f75fdf-fdea-473f-a156-bf1ed60acf58", SubmissionID: 99, AttemptNo: 1, ProblemID: 42, UserID: 7, Language: "go"}
}

type resultRecorder struct {
	result callback.Result
	calls  int
}

func (recorder *resultRecorder) Publish(_ context.Context, result callback.Result) (callback.Disposition, error) {
	recorder.calls++
	recorder.result = result
	return callback.DispositionApplied, nil
}
