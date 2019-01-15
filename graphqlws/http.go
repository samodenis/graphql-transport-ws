package graphqlws

import (
	"net/http"

	"github.com/gorilla/websocket"

	"github.com/samodenis/graphql-transport-ws/graphqlws/internal/connection"
	"context"
)

type AuthValidator interface {
	CheckAuth(r *http.Request, ctx context.Context) (context.Context, error)
}

const protocolGraphQLWS = "graphql-ws"

var upgrader = websocket.Upgrader{
	CheckOrigin:  func(r *http.Request) bool { return true },
	Subprotocols: []string{protocolGraphQLWS},
}

// NewHandlerFunc returns an http.HandlerFunc that supports GraphQL over websockets
func NewHandlerFunc(rootCtx context.Context, svc connection.GraphQLService, httpHandler http.Handler, authValidator AuthValidator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, subprotocol := range websocket.Subprotocols(r) {
			if subprotocol == "graphql-ws" {
				ctx, err := authValidator.CheckAuth(r, rootCtx)
				if err != nil {
					return
				}
				ws, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}

				if ws.Subprotocol() != protocolGraphQLWS {
					ws.Close()
					return
				}

				go connection.Connect(ws, svc, ctx)
				return
			}
		}

		// Fallback to HTTP
		httpHandler.ServeHTTP(w, r)
	}
}
