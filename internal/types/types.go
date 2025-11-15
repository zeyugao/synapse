package types

import (
	"net/http"

	pb "github.com/zeyugao/synapse/internal/types/proto"
)

type (
	ModelInfo            = pb.ModelInfo
	ModelsResponse       = pb.ModelsResponse
	ClientRegistration   = pb.ClientRegistration
	ForwardRequest       = pb.ForwardRequest
	ForwardResponse      = pb.ForwardResponse
	ModelProcessingStats = pb.ModelProcessingStats
	ModelUpdateRequest   = pb.ModelUpdateRequest
	UnregisterRequest    = pb.UnregisterRequest
	ForceShutdownRequest = pb.ForceShutdownRequest
	ServerMessage        = pb.ServerMessage
	ClientMessage        = pb.ClientMessage
	HeaderEntry          = pb.HeaderEntry
	ResponseKind         = pb.ResponseKind
	Heartbeat            = pb.Heartbeat
	ClientClose          = pb.ClientClose
	Pong                 = pb.Pong
)

const (
	ResponseKindUnspecified = pb.ResponseKind_RESPONSE_KIND_UNSPECIFIED
	ResponseKindNormal      = pb.ResponseKind_RESPONSE_KIND_NORMAL
	ResponseKindStream      = pb.ResponseKind_RESPONSE_KIND_STREAM
)

// HTTPHeaderToProto converts an http.Header to protobuf header entries.
func HTTPHeaderToProto(header http.Header) []*pb.HeaderEntry {
	if header == nil {
		return nil
	}

	entries := make([]*pb.HeaderEntry, 0, len(header))
	for key, values := range header {
		entry := &pb.HeaderEntry{
			Key:    key,
			Values: append([]string(nil), values...),
		}
		entries = append(entries, entry)
	}
	return entries
}

// ProtoToHTTPHeader converts protobuf header entries back to http.Header.
func ProtoToHTTPHeader(entries []*pb.HeaderEntry) http.Header {
	if len(entries) == 0 {
		return http.Header{}
	}

	header := make(http.Header, len(entries))
	for _, entry := range entries {
		if entry == nil {
			continue
		}
		values := append([]string(nil), entry.Values...)
		header[entry.Key] = values
	}
	return header
}
