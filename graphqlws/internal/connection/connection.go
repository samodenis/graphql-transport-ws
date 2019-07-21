package connection

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"time"
	"github.com/graph-gophers/graphql-go"
)

type operationMessageType string

// https://github.com/apollographql/subscriptions-transport-ws/blob/a56491c6feacd96cab47b7a3df8c2cb1b6a96e36/src/message-types.ts
const (
	typeComplete            operationMessageType = "complete"
	typeConnectionAck       operationMessageType = "connection_ack"
	typeConnectionError     operationMessageType = "connection_error"
	typeConnectionInit      operationMessageType = "connection_init"
	typeConnectionKeepAlive operationMessageType = "ka"
	typeConnectionTerminate operationMessageType = "connection_terminate"
	typeData                operationMessageType = "data"
	typeError               operationMessageType = "error"
	typeStart               operationMessageType = "start"
	typeStop                operationMessageType = "stop"
	typePing                operationMessageType = "ping"
	typeReceive             operationMessageType = "receive"
	typePong                operationMessageType = "pong"
)

type wsConnection interface {
	Close() error
	ReadJSON(v interface{}) error
	SetReadLimit(limit int64)
	SetWriteDeadline(t time.Time) error
	WriteJSON(v interface{}) error
}

type sendFunc func(id string, omType operationMessageType, payload json.RawMessage)

// TODO?: omitempty?
type operationMessage struct {
	ID      string               `json:"id,omitempty"`
	Payload json.RawMessage      `json:"payload,omitempty"`
	Type    operationMessageType `json:"type"`
}

type startMessagePayload struct {
	OperationName string                 `json:"operationName"`
	Query         string                 `json:"query"`
	Variables     map[string]interface{} `json:"variables"`
}

type receiveMessagePayload struct{
	ID	string	`json:"id"`
}

type initMessagePayload struct{}

// GraphQLService interface
type GraphQLService interface {
	Subscribe(ctx context.Context, document string, operationName string, variableValues map[string]interface{}) (payloads <-chan interface{}, err error)
	Exec(ctx context.Context, queryString string, operationName string, variables map[string]interface{}) *graphql.Response
}

type connection struct {
	cancel       func()
	service      GraphQLService
	writeTimeout time.Duration
	ws           wsConnection
}

// ReadLimit limits the maximum size of incoming messages
func ReadLimit(limit int64) func(conn *connection) {
	return func(conn *connection) {
		conn.ws.SetReadLimit(limit)
	}
}

// WriteTimeout sets a timeout for outgoing messages
func WriteTimeout(d time.Duration) func(conn *connection) {
	return func(conn *connection) {
		conn.writeTimeout = d
	}
}

// Connect implements the apollographql subscriptions-transport-ws protocol@v0.9.4
// https://github.com/apollographql/subscriptions-transport-ws/blob/v0.9.4/PROTOCOL.md
func Connect(ws wsConnection, service GraphQLService, rootCtx context.Context, options ...func(conn *connection)) func() {
	conn := &connection{
		service: service,
		ws:      ws,
	}

	defaultOpts := []func(conn *connection){
		ReadLimit(4096),
		WriteTimeout(time.Second),
	}

	for _, opt := range append(defaultOpts, options...) {
		opt(conn)
	}

	ctx, cancel := context.WithCancel(rootCtx)
	conn.cancel = cancel
	conn.readLoop(ctx, conn.writeLoop(ctx))

	return cancel
}

func (conn *connection) writeLoop(ctx context.Context) sendFunc {
	stop := make(chan struct{})
	out := make(chan *operationMessage)

	send := func(id string, omType operationMessageType, payload json.RawMessage) {
		select {
		case <-stop:
			return
		case out <- &operationMessage{ID: id, Type: omType, Payload: payload}:
		}
	}

	go func() {
		defer close(stop)
		defer conn.close()

		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-out:
				select {
				case <-ctx.Done():
					return
				default:
				}

				if err := conn.ws.SetWriteDeadline(time.Now().Add(conn.writeTimeout)); err != nil {
					return
				}

				if err := conn.ws.WriteJSON(msg); err != nil {
					return
				}
			}
		}
	}()

	return send
}

// TODO?: export this instead of returning a simple func from Connect()
func (conn *connection) close() {
	conn.cancel()
	conn.ws.Close()
}

func (conn *connection) readLoop(ctx context.Context, send sendFunc) {
	defer conn.close()

	opDone := map[string]func(){}
	for {
		var msg operationMessage
		err := conn.ws.ReadJSON(&msg)
		if err != nil {
			return
		}

		switch msg.Type {
		case typeConnectionInit:
			var initMsg initMessagePayload
			if err := json.Unmarshal(msg.Payload, &initMsg); err != nil {
				ep := errPayload(fmt.Errorf("invalid payload for type: %s", msg.Type))
				send("", typeConnectionError, ep)
				continue
			}
			send("", typeConnectionAck, nil)

		case typeStart:
			// TODO: check an operation with the same ID hasn't been started already
			if msg.ID == "" {
				ep := errPayload(errors.New("missing ID for start operation"))
				send("", typeConnectionError, ep)
				continue
			}

			var osp startMessagePayload
			if err := json.Unmarshal(msg.Payload, &osp); err != nil {
				ep := errPayload(fmt.Errorf("invalid payload for type: %s", msg.Type))
				send(msg.ID, typeConnectionError, ep)
				continue
			}

			opCtx, cancel := context.WithCancel(ctx)
			jsonBytes, _ := json.Marshal(opCtx)
			hasher := sha1.New()
			hasher.Write(jsonBytes)
			uniqID := generateRandomString(16) + "_" + base64.URLEncoding.EncodeToString(hasher.Sum(nil))

			opCtx = context.WithValue(opCtx, "socket_id", uniqID)
			// TODO: timeout this call, to guard against poor clients
			c, err := conn.service.Subscribe(opCtx, osp.Query, osp.OperationName, osp.Variables)
			if err != nil {
				cancel()
				send(msg.ID, typeError, errPayload(err))
				send(msg.ID, typeComplete, nil)
				continue
			}

			opDone[msg.ID] = cancel

			go func() {
				defer cancel()
				for {
					select {
					case <-opCtx.Done():
						return
					case payload, more := <-c:
						if !more {
							send(msg.ID, typeComplete, nil)
							return
						}

						jsonPayload, err := json.Marshal(payload)
						if err != nil {
							send(msg.ID, typeError, errPayload(err))
							continue
						}
						send(msg.ID, typeData, jsonPayload)
					}
				}
			}()

		case typeStop:
			onDone, ok := opDone[msg.ID]
			if ok {
				delete(opDone, msg.ID)
				onDone()
			}
			send(msg.ID, typeComplete, nil)

		case typePing:
			response := conn.service.Exec(ctx, "{check_subscription}", "", nil)
			responseJSON, err := json.Marshal(response)
			if err != nil {
				send(msg.ID, typeError, errPayload(err))
				continue
			}
			send("", typePong, responseJSON)

		case typeReceive:
			var rp receiveMessagePayload
			if err := json.Unmarshal(msg.Payload, &rp); err != nil {
				send(msg.ID, typeError, errPayload(err))
				continue
			}

			response := conn.service.Exec(ctx, fmt.Sprintf("mutation {receive_socket_event(id:%s)}", rp.ID), "", nil)
			responseJSON, err := json.Marshal(response)
			if err != nil {
				send(msg.ID, typeError, errPayload(err))
				continue
			}
			send("", typePong, responseJSON)

		case typeConnectionTerminate:
			return

		default:
			ep := errPayload(fmt.Errorf("unknown operation message of type: %s", msg.Type))
			send(msg.ID, typeError, ep)
		}
	}
}

func errPayload(err error) json.RawMessage {
	b, _ := json.Marshal(struct {
		Message string `json:"message"`
	}{
		Message: err.Error(),
	})
	return b
}

func generateRandomString(length int) string {
	rand.Seed(time.Now().UnixNano())
	digits := "0123456789"
	all := "ABCDEFGHIJKLMNOPQRSTUVWXYZ" +
		"abcdefghijklmnopqrstuvwxyz" +
		digits
	buf := make([]byte, length)
	buf[0] = all[rand.Intn(len(digits))]
	for i := 1; i < length; i++ {
		buf[i] = all[rand.Intn(len(all))]
	}
	rand.Shuffle(len(buf), func(i, j int) {
		buf[i], buf[j] = buf[j], buf[i]
	})
	str := string(buf)

	return str
}
