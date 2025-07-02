package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"

	envoy_config_core_v3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	exppb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoy_type_v3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const maxRequestsPerUser = 5

// authServer holds the loaded API keys and an in-memory counter for user queries.
type authServer struct {
	exppb.UnimplementedExternalProcessorServer
	keys        map[string]string
	mu          sync.RWMutex
	userQueries map[string]uint64
}

// newAuthServer creates a new server instance after loading keys from a file.
func newAuthServer(keyFilePath string) (*authServer, error) {
	file, err := os.Open(keyFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open key file %q: %w", keyFilePath, err)
	}
	defer file.Close()

	keys := make(map[string]string)
	scanner := bufio.NewScanner(file)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) != 2 {
			log.Printf("Warning: malformed line #%d in key file, skipping.", lineNum)
			continue
		}
		apiKey := strings.TrimSpace(parts[0])
		userName := strings.TrimSpace(parts[1])
		if apiKey != "" && userName != "" {
			keys[apiKey] = userName
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading key file: %w", err)
	}

	log.Printf("Loaded %d API keys.", len(keys))
	return &authServer{
		keys:        keys,
		userQueries: make(map[string]uint64),
	}, nil
}

// Process is the main gRPC method that Envoy calls for each request.
func (s *authServer) Process(stream exppb.ExternalProcessor_ProcessServer) error {
	for {
		req, err := stream.Recv()
		if err != nil {
			if err == io.EOF || status.Code(err) == codes.Canceled {
				return nil
			}
			log.Printf("stream recv error: %v", err)
			return err
		}

		switch v := req.Request.(type) {
		case *exppb.ProcessingRequest_RequestHeaders:
			headers := v.RequestHeaders.GetHeaders().GetHeaders()
			apiKey := findHeaderValue(headers, "x-api-key")

			username, ok := s.keys[apiKey]
			if !ok {
				log.Printf("DENY: Request with missing or invalid API key.")
				if err := sendDenyResponse(stream, "Invalid or missing API Key.", envoy_type_v3.StatusCode_Unauthorized); err != nil {
					return err
				}
				return nil
			}

			// Check quota before incrementing.
			s.mu.Lock()
			currentCount := s.userQueries[username]

			if currentCount >= maxRequestsPerUser {
				log.Printf("DENY: Quota exceeded for user '%s'.", username)
				s.mu.Unlock()
				if err := sendDenyResponse(stream, "Daily quota has been reached and will reset at 4:00AM EST.", envoy_type_v3.StatusCode_TooManyRequests); err != nil {
					return err
				}
				return nil
			}

			// If quota is not exceeded, increment and proceed.
			s.userQueries[username]++
			newCount := s.userQueries[username]
			s.mu.Unlock()

			log.Printf("ALLOW: Request %d/%d for user '%s'.", newCount, maxRequestsPerUser, username)

			if err := sendContinueResponse(stream); err != nil {
				return err
			}

		default:
			if err := sendContinueResponse(stream); err != nil {
				return err
			}
		}
	}
}

// findHeaderValue is a helper to extract a header's value.
func findHeaderValue(headers []*envoy_config_core_v3.HeaderValue, key string) string {
	for _, h := range headers {
		if h.GetKey() == key {
			return string(h.GetRawValue())
		}
	}
	return ""
}

// sendContinueResponse tells Envoy to continue processing the request.
func sendContinueResponse(stream exppb.ExternalProcessor_ProcessServer) error {
	return stream.Send(&exppb.ProcessingResponse{
		Response: &exppb.ProcessingResponse_RequestHeaders{
			RequestHeaders: &exppb.HeadersResponse{},
		},
	})
}

// sendDenyResponse tells Envoy to stop and send an immediate response.
func sendDenyResponse(stream exppb.ExternalProcessor_ProcessServer, body string, code envoy_type_v3.StatusCode) error {
	return stream.Send(&exppb.ProcessingResponse{
		Response: &exppb.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &exppb.ImmediateResponse{
				Status: &envoy_type_v3.HttpStatus{
					Code: code,
				},
				Body: []byte(body),
			},
		},
	})
}

func main() {
	keyFile := os.Getenv("KEY_FILE")
	if keyFile == "" {
		log.Fatal("KEY_FILE environment variable not set.")
	}

	server, err := newAuthServer(keyFile)
	if err != nil {
		log.Fatalf("Failed to create auth server: %v", err)
	}

	lis, err := net.Listen("tcp", ":9000")
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	exppb.RegisterExternalProcessorServer(grpcServer, server)

	log.Println("auth-extproc listening on :9000")
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("Failed to serve: %v", err)
	}
}
