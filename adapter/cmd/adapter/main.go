package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"uipath-adapter/internal/auth"
	"uipath-adapter/internal/server"
	pb "uipath-adapter/proto"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	defaultPort = 50051
)

func main() {
	log.Println("🚀 UiPath Adapter Starting...")
	log.Println("=" + string(make([]byte, 60)))

	// 读取端口
	port := defaultPort
	if portEnv := os.Getenv("PORT"); portEnv != "" {
		fmt.Sscanf(portEnv, "%d", &port)
	}

	// 连接到主框架的 AccountService
	mainServiceAddr := os.Getenv("MAIN_SERVICE_ADDR")
	if mainServiceAddr == "" {
		// 默认 IPv4 loopback：gRPC 经 ::1 在部分环境下会挂起（TCP 连得上但 RPC 卡住），
		// 客户端侧的 HTTP 已是双栈；内部 service-to-service gRPC 走 IPv4 最稳。
		mainServiceAddr = "127.0.0.1:50052"
	}

	log.Printf("📡 Connecting to main service at %s...", mainServiceAddr)
	conn, err := grpc.NewClient(mainServiceAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("❌ Failed to connect to main service: %v", err)
	}
	defer conn.Close()

	storageClient := pb.NewStorageServiceClient(conn)
	accountClient := pb.NewAccountServiceClient(conn)

	// 创建账号源服务
	accountSource := server.NewUiPathAccountSource(storageClient)

	// 从环境变量读取凭据
	email := os.Getenv("UIPATH_EMAIL")
	password := os.Getenv("UIPATH_PASSWORD")
	envOrgID := os.Getenv("UIPATH_ORG_NAME")
	tenantID := os.Getenv("UIPATH_TENANT_NAME")
	if envOrgID == "" {
		envOrgID = "pg5001d3a6ef"
	}
	if tenantID == "" {
		tenantID = "DefaultTenant"
	}

	if email == "" || password == "" {
		log.Println("⚠️  No UIPATH_EMAIL/PASSWORD configured; adapter runs without real backend")
	} else {
		// 登录窗口保护：先置位 pending，使注册后到 web session 就绪前，框架 5s 水位 ticker
		// 触发的 SupplyAccounts 不走全量密码登录回退（避免撞反爬）。登录完成后清除。
		accountSource.SetWebLoginPending(true)
	}

	// 创建监听器（双栈 IPv4+IPv6：优先 [::]，失败回退 0.0.0.0）
	listener, err := listenDualStack(port)
	if err != nil {
		log.Fatalf("❌ Failed to listen: %v", err)
	}

	// 创建 gRPC 服务器
	grpcServer := grpc.NewServer()

	// 注册 AdapterService
	adapter, err := server.NewUiPathAdapterWithAccountClient(accountClient)
	if err != nil {
		log.Fatalf("❌ Failed to create adapter: %v", err)
	}
	pb.RegisterAdapterServiceServer(grpcServer, adapter)

	// 注册 AccountSourceService
	pb.RegisterAccountSourceServiceServer(grpcServer, accountSource)

	log.Printf("✅ UiPath Adapter started on port %d", port)
	log.Println("📡 Services: AdapterService, AccountSourceService")

	// 先启动本机 gRPC 服务，再向主框架注册。
	// 主框架在 RegisterAccountSource 里会立刻回调 ListAccounts；
	// 如果这里晚于注册才 Serve，主框架会一直等到超时。
	go func() {
		if err := grpcServer.Serve(listener); err != nil {
			log.Fatalf("❌ Failed to serve: %v", err)
		}
	}()

	if err := waitForPort(fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second); err != nil {
		log.Fatalf("❌ Adapter gRPC service not ready: %v", err)
	}

	// 注册到主框架
	log.Println("📝 Registering account source to main service...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	registerResp, err := accountClient.RegisterAccountSource(ctx, &pb.RegisterSourceRequest{
		SourceId:     "uipath-adapter-1",
		Provider:     "uipath",
		CallbackAddr: fmt.Sprintf("127.0.0.1:%d", port),
		Capabilities: []string{"auto_refresh", "health_check", "supply"},
		Watermark: &pb.WatermarkConfig{
			MinAccounts:     2,
			MaxAccounts:     10,
			LowWatermark:    3,
			HighWatermark:   8,
			SupplyBatchSize: 2,
		},
	})

	// 注册响应携带框架下发的初始配置（含出站代理）。先于登录应用，
	// 使后续 AuthenticateWeb 的全部出站请求走代理规避反爬。
	initialProxy := ""
	if err != nil {
		log.Printf("⚠️  Failed to register account source: %v", err)
		log.Println("   Adapter will run without account pool integration")
	} else if registerResp.Success {
		log.Printf("✅ Account source registered: %s", registerResp.Message)
		if cfg := registerResp.GetConfig(); cfg != nil {
			initialProxy = cfg.GetProxyUrl()
			if initialProxy != "" {
				log.Printf("🔌 Initial outbound proxy from framework: %q", initialProxy)
			}
		}
	} else {
		log.Printf("⚠️  Registration failed: %s", registerResp.Message)
	}
	accountSource.SetProxy(initialProxy)

	// 登录：1 次密码登录拿全部 org → 为每个 org 注册 base account + 注入 web session。
	// 此时已应用代理，登录出站走代理。失败则回退单 base account。
	if email != "" && password != "" {
		authMgr, aerr := auth.NewUiPathAuth(email, password, initialProxy)
		if aerr != nil {
			log.Printf("❌ Failed to create auth manager: %v (fallback single base account)", aerr)
			accountSource.AddBaseAccount(email, password, envOrgID, tenantID)
		} else {
			loginCtx, lcancel := context.WithTimeout(context.Background(), 60*time.Second)
			orgs, lerr := authMgr.AuthenticateWeb(loginCtx)
			lcancel()
			if lerr != nil || len(orgs) == 0 {
				log.Printf("⚠️  AuthenticateWeb failed (%v); fallback single base account", lerr)
				accountSource.AddBaseAccount(email, password, envOrgID, tenantID)
			} else {
				log.Printf("✅ Web login ok: %d orgs visible", len(orgs))
				for _, org := range orgs {
					accountSource.AddBaseAccount(email, password, org.GlobalID, tenantID)
				}
				accountSource.SetWebSession(authMgr, email, orgs)
			}
		}
		// 登录窗口结束（无论成功失败）：解除 pending，恢复正常补号路径。
		accountSource.SetWebLoginPending(false)

		// 心跳 goroutine：周期上报存活并接收最新配置（代理）热更新。
		go runHeartbeat(accountClient, accountSource, "uipath-adapter-1")
	}

	log.Println("")

	// 处理优雅关闭
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan

		log.Println("")
		log.Println("🛑 Shutting down gracefully...")
		grpcServer.GracefulStop()
		log.Println("✅ Shutdown complete")
	}()

	// 阻塞主 goroutine
	select {}
}

// runHeartbeat 周期向框架上报存活，并用响应里的最新配置热更新出站代理。
// 框架据心跳维持实例 active；超时未上报则被 reaper 标记 stale。
func runHeartbeat(client pb.AccountServiceClient, src *server.UiPathAccountSource, sourceID string) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		resp, err := client.Heartbeat(ctx, &pb.HeartbeatRequest{
			SourceId:   sourceID,
			InstanceId: sourceID,
			Status:     "active",
		})
		cancel()
		if err != nil {
			log.Printf("⚠️  Heartbeat failed: %v", err)
			continue
		}
		if cfg := resp.GetConfig(); cfg != nil {
			src.SetProxy(cfg.GetProxyUrl())
		}
	}
}

// listenDualStack 在指定端口监听，支持 IPv4 与 IPv6 双栈：
// 优先绑 [::]（Linux 默认双栈，同时收 v4+v6），失败回退 0.0.0.0（纯 IPv4）。
// 这样主框架无论用 127.0.0.1、::1 还是 localhost 连 adapter 都能通。
func listenDualStack(port int) (net.Listener, error) {
	if lis, err := net.Listen("tcp", fmt.Sprintf("[::]:%d", port)); err == nil {
		return lis, nil
	}
	return net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
}

// waitForPort 等待指定 TCP 端口可连接，避免过早发起跨进程回调。
func waitForPort(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s", addr)
}
