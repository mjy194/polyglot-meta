package server

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"uipath-adapter/internal/client"
	"uipath-adapter/internal/converter"
	pb "uipath-adapter/proto"
)

// UiPathAdapter 实现 AdapterService 接口
type UiPathAdapter struct {
	pb.UnimplementedAdapterServiceServer
	client         *client.UiPathClient
	sessions       sync.Map // session_id -> uipath_session_id
	storageClient  pb.StorageServiceClient
	accountClient  pb.AccountServiceClient
	useAccountPool bool
}

// NewUiPathAdapter 创建 UiPath Adapter 实例（固定账号模式，已废弃，建议使用账号池）
func NewUiPathAdapter() (*UiPathAdapter, error) {
	// 连接到主服务的 StorageService
	storageAddr := os.Getenv("STORAGE_SERVICE_ADDR")
	if storageAddr == "" {
		storageAddr = "localhost:50052"
	}

	conn, err := grpc.NewClient(storageAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Printf("⚠️  Failed to connect to storage service: %v\n", err)
	}

	var storageClient pb.StorageServiceClient
	if conn != nil {
		storageClient = pb.NewStorageServiceClient(conn)
	}

	accessToken := os.Getenv("UIPATH_ACCESS_TOKEN")

	if accessToken == "" {
		log.Println("⚠️  No credentials found, using mock mode")
		return &UiPathAdapter{client: nil, storageClient: storageClient}, nil
	}

	baseURL := os.Getenv("UIPATH_BASE_URL")
	if baseURL == "" {
		baseURL = "https://cloud.uipath.com/pg5001d3a6ef/DefaultTenant/autopilotstudio_/autopilot-everywhere"
	}

	return &UiPathAdapter{
		client:        client.NewUiPathClient(baseURL, accessToken),
		storageClient: storageClient,
	}, nil
}

// NewUiPathAdapterWithAccountClient 使用账号池创建 Adapter
func NewUiPathAdapterWithAccountClient(accountClient pb.AccountServiceClient) (*UiPathAdapter, error) {
	log.Println("🎯 Creating UiPath Adapter with account pool integration")

	return &UiPathAdapter{
		client:         nil,
		storageClient:  nil,
		accountClient:  accountClient,
		useAccountPool: true,
	}, nil
}

// GetMetadata 返回 Adapter 元数据
func (a *UiPathAdapter) GetMetadata(ctx context.Context, req *pb.GetMetadataRequest) (*pb.AdapterMetadata, error) {
	return &pb.AdapterMetadata{
		Name:    "uipath-adapter",
		Version: "1.0.0",
		SupportedModels: []string{
			"claude-opus-4-8",
			"claude-sonnet-4-6",
			"claude-haiku-4-5",
		},
		Capabilities: &pb.AdapterCapabilities{
			Streaming:     true,
			ToolUse:       true,
			Vision:        true,
			PromptCaching: false,
			MaxTokens:     100000,
			MaxConcurrent: 5,
		},
		Metadata: map[string]string{
			"backend": "uipath",
			"region":  "us",
		},
		NativeProtocols: nativeProtocolSupport(),
	}, nil
}

// HealthCheck 健康检查
func (a *UiPathAdapter) HealthCheck(ctx context.Context, req *pb.HealthCheckRequest) (*pb.HealthCheckResponse, error) {
	status := pb.HealthCheckResponse_HEALTHY
	message := "UiPath adapter is healthy"

	if !a.useAccountPool && a.client == nil {
		status = pb.HealthCheckResponse_DEGRADED
		message = "Running in mock mode"
	}

	return &pb.HealthCheckResponse{
		Status:         status,
		Message:        message,
		UptimeSeconds:  0,
		ActiveRequests: 0,
		ErrorRate:      0,
	}, nil
}

// ProcessRequest 处理请求（流式）
func (a *UiPathAdapter) ProcessRequest(req *pb.UniversalRequest, stream pb.AdapterService_ProcessRequestServer) error {
	log.Printf("🔄 ProcessRequest: request_id=%s, model=%s\n", req.RequestId, req.Model)

	return a.processUniversalRequest(req, stream.Context(), stream.Send)
}

func (a *UiPathAdapter) processUniversalRequest(req *pb.UniversalRequest, ctx context.Context, send func(*pb.UniversalResponse) error) error {
	// 账号池模式
	if a.useAccountPool {
		return a.processRealRequestWithPool(req, ctx, send)
	}

	// Mock 模式
	if a.client == nil {
		return a.processMockRequest(req, send)
	}

	// 固定账号模式
	return a.processRealRequest(req, ctx, send)
}

func (a *UiPathAdapter) ProcessNative(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer) error {
	log.Printf("🔄 ProcessNative: request_id=%s, protocol=%s, endpoint=%s\n", req.RequestId, req.Protocol, req.Endpoint)

	handler, ok := nativeHandlerFor(req.Protocol, req.Endpoint)
	if !ok {
		return stream.Send(&pb.NativeResponse{
			RequestId:  req.RequestId,
			StatusCode: 400,
			Headers:    map[string]string{"Content-Type": "application/json"},
			Body:       []byte(`{"error":{"message":"unsupported native protocol endpoint","type":"invalid_request_error"}}`),
			EndStream:  true,
		})
	}
	return handler.Process(a, req, stream)
}

// CancelRequest 取消请求
func (a *UiPathAdapter) CancelRequest(ctx context.Context, req *pb.CancelRequestRequest) (*pb.CancelRequestResponse, error) {
	return &pb.CancelRequestResponse{
		Cancelled: true,
		Message:   "Request cancelled",
	}, nil
}

// processMockRequest Mock 模式处理
func (a *UiPathAdapter) processMockRequest(req *pb.UniversalRequest, send func(*pb.UniversalResponse) error) error {
	if err := send(&pb.UniversalResponse{
		RequestId: req.RequestId,
		Response: &pb.UniversalResponse_Chunk{
			Chunk: &pb.ContentChunk{Text: "Mock response from UiPath Adapter", Index: 0},
		},
	}); err != nil {
		return err
	}

	if err := send(&pb.UniversalResponse{
		RequestId: req.RequestId,
		Response: &pb.UniversalResponse_Completion{
			Completion: &pb.CompletionInfo{FinishReason: "stop", InputTokens: 10, OutputTokens: 20},
		},
	}); err != nil {
		return err
	}

	return nil
}

// processRealRequest 固定账号模式处理
func (a *UiPathAdapter) processRealRequest(req *pb.UniversalRequest, ctx context.Context, send func(*pb.UniversalResponse) error) error {
	sessionID := req.RequestId
	uipathSessionID, err := a.getOrCreateSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}

	uipathReq, err := converter.UniversalToUiPath(req)
	if err != nil {
		return fmt.Errorf("failed to convert request: %w", err)
	}

	eventChan, err := a.client.SendMessage(ctx, uipathSessionID, uipathReq)
	if err != nil {
		return fmt.Errorf("failed to call UiPath API: %w", err)
	}

	for event := range eventChan {
		univResponses, err := converter.UiPathToUniversalResponses(req.RequestId, event)
		if err != nil || len(univResponses) == 0 {
			continue
		}

		for _, univResp := range univResponses {
			if err := send(univResp); err != nil {
				return fmt.Errorf("failed to send response: %w", err)
			}
		}
	}

	return nil
}

// getOrCreateSession 获取或创建 UiPath session
func (a *UiPathAdapter) getOrCreateSession(ctx context.Context, sessionKey string) (string, error) {
	if val, ok := a.sessions.Load(sessionKey); ok {
		return val.(string), nil
	}

	uipathSessionID, err := a.client.CreateSession(ctx)
	if err != nil {
		return "", err
	}

	a.sessions.Store(sessionKey, uipathSessionID)
	return uipathSessionID, nil
}

// processRealRequestWithPool 使用账号池处理请求
func (a *UiPathAdapter) processRealRequestWithPool(req *pb.UniversalRequest, ctx context.Context, send func(*pb.UniversalResponse) error) error {
	sessionID := ""
	if req.Context != nil {
		sessionID = req.Context.SessionId
	}

	acquireResp, err := a.accountClient.AcquireAccount(ctx, &pb.AcquireAccountRequest{
		Provider:  "uipath",
		SessionId: sessionID,
	})

	if err != nil {
		return fmt.Errorf("acquire account from pool: %w", err)
	}
	if !acquireResp.Available {
		return fmt.Errorf("no available account in pool (provider=uipath)")
	}

	accountID := acquireResp.AccountId
	accessToken := acquireResp.Credentials["access_token"]
	upstreamURL := acquireResp.Credentials["upstream_url"]

	log.Printf("🔑 Acquired account %s from pool", accountID)

	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.accountClient.ReleaseAccount(releaseCtx, &pb.ReleaseAccountRequest{AccountId: accountID})
		log.Printf("🔓 Released account %s", accountID)
	}()

	uipathClient := client.NewUiPathClient(upstreamURL, accessToken)
	if req.Context != nil {
		uipathClient.SetProxy(req.Context.GetProxyUrl())
	}

	uipathSessionID, err := a.getOrCreateSessionWithClient(ctx, req.RequestId, uipathClient)
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}

	uipathReq, err := converter.UniversalToUiPath(req)
	if err != nil {
		return fmt.Errorf("failed to convert request: %w", err)
	}

	eventChan, err := uipathClient.SendMessage(ctx, uipathSessionID, uipathReq)
	if err != nil {
		return fmt.Errorf("failed to call UiPath API: %w", err)
	}

	var totalTokens int64 = 0

	for event := range eventChan {
		univResponses, err := converter.UiPathToUniversalResponses(req.RequestId, event)
		if err != nil || len(univResponses) == 0 {
			continue
		}

		for _, univResp := range univResponses {
			if err := send(univResp); err != nil {
				return fmt.Errorf("failed to send response: %w", err)
			}

			if comp := univResp.GetCompletion(); comp != nil {
				totalTokens = int64(comp.InputTokens + comp.OutputTokens)
			}
		}
	}

	if totalTokens > 0 {
		reportCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		a.accountClient.ReportUsage(reportCtx, &pb.ReportUsageRequest{
			AccountId:     accountID,
			TokensUsed:    totalTokens,
			RequestsCount: 1,
		})
	}

	return nil
}

// getOrCreateSessionWithClient 使用指定 client 获取或创建 session
func (a *UiPathAdapter) getOrCreateSessionWithClient(ctx context.Context, sessionKey string, uipathClient *client.UiPathClient) (string, error) {
	if val, ok := a.sessions.Load(sessionKey); ok {
		return val.(string), nil
	}

	uipathSessionID, err := uipathClient.CreateSession(ctx)
	if err != nil {
		return "", err
	}

	a.sessions.Store(sessionKey, uipathSessionID)
	return uipathSessionID, nil
}
