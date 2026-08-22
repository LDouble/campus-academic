package rpc

import (
	"context"

	"github.com/LDouble/campus-academic/internal/core/requestmeta"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const requestIDMetadataKey = "x-request-id"

func requestIDClientInterceptor(
	ctx context.Context,
	method string,
	req any,
	reply any,
	connection *grpc.ClientConn,
	invoker grpc.UnaryInvoker,
	options ...grpc.CallOption,
) error {
	if requestID := requestmeta.RequestID(ctx); requestID != "" {
		outgoing, _ := metadata.FromOutgoingContext(ctx)
		outgoing = outgoing.Copy()
		outgoing.Set(requestIDMetadataKey, requestID)
		ctx = metadata.NewOutgoingContext(ctx, outgoing)
	}
	return invoker(ctx, method, req, reply, connection, options...)
}

func requestIDServerInterceptor(
	ctx context.Context,
	req any,
	_ *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (any, error) {
	if incoming, ok := metadata.FromIncomingContext(ctx); ok {
		values := incoming.Get(requestIDMetadataKey)
		if len(values) == 1 {
			ctx = requestmeta.WithRequestID(ctx, values[0])
		}
	}
	return handler(ctx, req)
}
