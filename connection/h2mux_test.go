package connection

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/cloudflare/cloudflared/h2mux"
	"github.com/gobwas/ws/wsutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	testMuxerConfig = &MuxerConfig{
		HeartbeatInterval:  time.Second * 5,
		MaxHeartbeats:      5,
		CompressionSetting: 0,
		MetricsUpdateFreq:  time.Second * 5,
	}
)

func newH2MuxConnection(ctx context.Context, t require.TestingT) (*h2muxConnection, *h2mux.Muxer) {
	edgeConn, originConn := net.Pipe()
	edgeMuxChan := make(chan *h2mux.Muxer)
	go func() {
		edgeMuxConfig := h2mux.MuxerConfig{
			Logger: testObserver,
		}
		edgeMux, err := h2mux.Handshake(edgeConn, edgeConn, edgeMuxConfig, h2mux.ActiveStreams)
		require.NoError(t, err)
		edgeMuxChan <- edgeMux
	}()
	var connIndex = uint8(0)
	h2muxConn, err, _ := NewH2muxConnection(ctx, testConfig, testMuxerConfig, originConn, connIndex, testObserver)
	require.NoError(t, err)
	return h2muxConn, <-edgeMuxChan
}

func TestServeStreamHTTP(t *testing.T) {
	tests := []testRequest{
		{
			name:           "ok",
			endpoint:       "/ok",
			expectedStatus: http.StatusOK,
			expectedBody:   []byte(http.StatusText(http.StatusOK)),
		},
		{
			name:           "large_file",
			endpoint:       "/large_file",
			expectedStatus: http.StatusOK,
			expectedBody:   testLargeResp,
		},
		{
			name:           "Bad request",
			endpoint:       "/400",
			expectedStatus: http.StatusBadRequest,
			expectedBody:   []byte(http.StatusText(http.StatusBadRequest)),
		},
		{
			name:           "Internal server error",
			endpoint:       "/500",
			expectedStatus: http.StatusInternalServerError,
			expectedBody:   []byte(http.StatusText(http.StatusInternalServerError)),
		},
		{
			name:           "Proxy error",
			endpoint:       "/error",
			expectedStatus: http.StatusBadGateway,
			expectedBody:   nil,
			isProxyError:   true,
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	h2muxConn, edgeMux := newH2MuxConnection(ctx, t)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		edgeMux.Serve(ctx)
	}()
	go func() {
		defer wg.Done()
		err := h2muxConn.serveMuxer(ctx)
		require.Error(t, err)
	}()

	for _, test := range tests {
		headers := []h2mux.Header{
			{
				Name:  ":path",
				Value: test.endpoint,
			},
		}
		stream, err := edgeMux.OpenStream(ctx, headers, nil)
		require.NoError(t, err)
		require.True(t, hasHeader(stream, ":status", strconv.Itoa(test.expectedStatus)))

		if test.isProxyError {
			assert.True(t, hasHeader(stream, responseMetaHeaderField, responseMetaHeaderCfd))
		} else {
			assert.True(t, hasHeader(stream, responseMetaHeaderField, responseMetaHeaderOrigin))
			body := make([]byte, len(test.expectedBody))
			_, err = stream.Read(body)
			require.NoError(t, err)
			require.Equal(t, test.expectedBody, body)
		}
	}
	cancel()
	wg.Wait()
}

func TestServeStreamWS(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h2muxConn, edgeMux := newH2MuxConnection(ctx, t)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		edgeMux.Serve(ctx)
	}()
	go func() {
		defer wg.Done()
		err := h2muxConn.serveMuxer(ctx)
		require.Error(t, err)
	}()

	headers := []h2mux.Header{
		{
			Name:  ":path",
			Value: "/ws",
		},
		{
			Name:  "connection",
			Value: "upgrade",
		},
		{
			Name:  "upgrade",
			Value: "websocket",
		},
	}

	readPipe, writePipe := io.Pipe()
	stream, err := edgeMux.OpenStream(ctx, headers, readPipe)
	require.NoError(t, err)

	require.True(t, hasHeader(stream, ":status", strconv.Itoa(http.StatusSwitchingProtocols)))
	assert.True(t, hasHeader(stream, responseMetaHeaderField, responseMetaHeaderOrigin))

	data := []byte("test websocket")
	err = wsutil.WriteClientText(writePipe, data)
	require.NoError(t, err)

	respBody, err := wsutil.ReadServerText(stream)
	require.NoError(t, err)
	require.Equal(t, data, respBody, fmt.Sprintf("Expect %s, got %s", string(data), string(respBody)))

	cancel()
	wg.Wait()
}

func hasHeader(stream *h2mux.MuxedStream, name, val string) bool {
	for _, header := range stream.Headers {
		if header.Name == name && header.Value == val {
			return true
		}
	}
	return false
}

func benchmarkServeStreamHTTPSimple(b *testing.B, test testRequest) {
	ctx, cancel := context.WithCancel(context.Background())
	h2muxConn, edgeMux := newH2MuxConnection(ctx, b)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		edgeMux.Serve(ctx)
	}()
	go func() {
		defer wg.Done()
		err := h2muxConn.serveMuxer(ctx)
		require.Error(b, err)
	}()

	headers := []h2mux.Header{
		{
			Name:  ":path",
			Value: test.endpoint,
		},
	}

	body := make([]byte, len(test.expectedBody))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StartTimer()
		stream, openstreamErr := edgeMux.OpenStream(ctx, headers, nil)
		_, readBodyErr := stream.Read(body)
		b.StopTimer()

		require.NoError(b, openstreamErr)
		assert.True(b, hasHeader(stream, responseMetaHeaderField, responseMetaHeaderOrigin))
		require.True(b, hasHeader(stream, ":status", strconv.Itoa(http.StatusOK)))
		require.NoError(b, readBodyErr)
		require.Equal(b, test.expectedBody, body)
	}

	cancel()
	wg.Wait()
}

func BenchmarkServeStreamHTTPSimple(b *testing.B) {
	test := testRequest{
		name:           "ok",
		endpoint:       "/ok",
		expectedStatus: http.StatusOK,
		expectedBody:   []byte(http.StatusText(http.StatusOK)),
	}

	benchmarkServeStreamHTTPSimple(b, test)
}

func BenchmarkServeStreamHTTPLargeFile(b *testing.B) {
	test := testRequest{
		name:           "large_file",
		endpoint:       "/large_file",
		expectedStatus: http.StatusOK,
		expectedBody:   testLargeResp,
	}

	benchmarkServeStreamHTTPSimple(b, test)
}
