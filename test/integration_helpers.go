package test

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/logger"
	livekit "github.com/livekit/protocol/proto"
	"github.com/livekit/protocol/utils"
	"github.com/twitchtv/twirp"

	"github.com/livekit/livekit-server/pkg/config"
	serverlogger "github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/testutils"
	testclient "github.com/livekit/livekit-server/test/client"
)

const (
	testApiKey        = "apikey"
	testApiSecret     = "apiSecret"
	testRoom          = "mytestroom"
	defaultServerPort = 7880
	secondServerPort  = 8880
	nodeId1           = "node-1"
	nodeId2           = "node-2"

	syncDelay = 100 * time.Millisecond
	// if there are deadlocks, it's helpful to set a short test timeout (i.e. go test -timeout=30s)
	// let connection timeout happen
	// connectTimeout = 5000 * time.Second
)

var (
	roomClient livekit.RoomService
)

func init() {
	serverlogger.InitDevelopment("")
}

func setupSingleNodeTest(name string, roomName string) (*service.LivekitServer, func()) {
	logger.Infow("----------------STARTING TEST----------------", "test", name)
	s := createSingleNodeServer()
	go func() {
		if err := s.Start(); err != nil {
			logger.Errorw("server returned error", err)
		}
	}()

	waitForServerToStart(s)

	// create test room
	_, err := roomClient.CreateRoom(contextWithCreateRoomToken(), &livekit.CreateRoomRequest{Name: roomName})
	if err != nil {
		panic(err)
	}
	return s, func() {
		s.Stop()
		logger.Infow("----------------FINISHING TEST----------------", "test", name)
	}
}

func setupMultiNodeTest(name string) (*service.LivekitServer, *service.LivekitServer, func()) {
	logger.Infow("----------------STARTING TEST----------------", "test", name)
	s1 := createMultiNodeServer(utils.NewGuid(nodeId1), defaultServerPort)
	s2 := createMultiNodeServer(utils.NewGuid(nodeId2), secondServerPort)
	go s1.Start()
	go s2.Start()

	waitForServerToStart(s1)
	waitForServerToStart(s2)

	return s1, s2, func() {
		s1.Stop()
		s2.Stop()
		redisClient().FlushAll(context.Background())
		logger.Infow("----------------FINISHING TEST----------------", "test", name)
	}
}

func contextWithCreateRoomToken() context.Context {
	header := make(http.Header)
	testclient.SetAuthorizationToken(header, createRoomToken())
	tctx, err := twirp.WithHTTPRequestHeaders(context.Background(), header)
	if err != nil {
		panic(err)
	}
	return tctx
}

func waitForServerToStart(s *service.LivekitServer) {
	// wait till ready
	ctx, cancel := context.WithTimeout(context.Background(), testutils.ConnectTimeout)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			panic("could not start server after timeout")
		case <-time.After(10 * time.Millisecond):
			if s.IsRunning() {
				return
			}
		}
	}
}

func waitUntilConnected(t *testing.T, clients ...*testclient.RTCClient) {
	logger.Infow("waiting for clients to become connected")
	wg := sync.WaitGroup{}
	for i := range clients {
		c := clients[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := c.WaitUntilConnected()
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
}

func createSingleNodeServer() *service.LivekitServer {
	var err error
	conf, err := config.NewConfig("", nil)
	if err != nil {
		panic(fmt.Sprintf("could not create config: %v", err))
	}
	conf.Development = true
	conf.Keys = map[string]string{testApiKey: testApiSecret}

	currentNode, err := routing.NewLocalNode(conf)
	if err != nil {
		panic(fmt.Sprintf("could not create local node: %v", err))
	}
	currentNode.Id = utils.NewGuid(nodeId1)

	s, err := service.InitializeServer(conf, currentNode)
	if err != nil {
		panic(fmt.Sprintf("could not create server: %v", err))
	}

	roomClient = livekit.NewRoomServiceJSONClient(fmt.Sprintf("http://localhost:%d", defaultServerPort), &http.Client{})
	return s
}

func createMultiNodeServer(nodeId string, port uint32) *service.LivekitServer {
	var err error
	conf, err := config.NewConfig("", nil)
	if err != nil {
		panic(fmt.Sprintf("could not create config: %v", err))
	}
	conf.Port = port
	conf.RTC.UDPPort = port + 1
	conf.RTC.TCPPort = port + 2
	conf.Redis.Address = "localhost:6379"
	conf.Development = true
	conf.Keys = map[string]string{testApiKey: testApiSecret}

	currentNode, err := routing.NewLocalNode(conf)
	if err != nil {
		panic(err)
	}
	currentNode.Id = nodeId

	// redis routing and store
	s, err := service.InitializeServer(conf, currentNode)
	if err != nil {
		panic(fmt.Sprintf("could not create server: %v", err))
	}

	roomClient = livekit.NewRoomServiceJSONClient(fmt.Sprintf("http://localhost:%d", port), &http.Client{})
	return s
}

// creates a client and runs against server
func createRTCClient(name string, port int, opts *testclient.Options) *testclient.RTCClient {
	token := joinToken(testRoom, name)
	ws, err := testclient.NewWebSocketConn(fmt.Sprintf("ws://localhost:%d", port), token, opts)
	if err != nil {
		panic(err)
	}

	c, err := testclient.NewRTCClient(ws)
	if err != nil {
		panic(err)
	}

	go c.Run()

	return c
}

func redisClient() *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
	})
}

func joinToken(room, name string) string {
	at := auth.NewAccessToken(testApiKey, testApiSecret).
		AddGrant(&auth.VideoGrant{RoomJoin: true, Room: room}).
		SetIdentity(name)
	t, err := at.ToJWT()
	if err != nil {
		panic(err)
	}
	return t
}

func createRoomToken() string {
	at := auth.NewAccessToken(testApiKey, testApiSecret).
		AddGrant(&auth.VideoGrant{RoomCreate: true}).
		SetIdentity("testuser")
	t, err := at.ToJWT()
	if err != nil {
		panic(err)
	}
	return t
}

func stopWriters(writers ...*testclient.TrackWriter) {
	for _, w := range writers {
		w.Stop()
	}
}

func stopClients(clients ...*testclient.RTCClient) {
	for _, c := range clients {
		c.Stop()
	}
}
