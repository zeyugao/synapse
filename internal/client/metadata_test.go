package client

import (
	"bytes"
	"fmt"
	"github.com/gorilla/websocket"
	"github.com/zeyugao/synapse/internal/types"
	"google.golang.org/protobuf/proto"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativeMetadataSurvivesDecodeAndWire(t *testing.T) {
	raw := `{"data":[{"id":"m","object":"model","created":123,"owned_by":"vllm","root":"/weights/model","parent":null,"max_model_len":262144,"large_integer":9007199254740993,"capabilities":{"vision":true,"disabled":false,"list":["a",null,{"x":1}]}}]}`
	models, e := decodeModelsResponse(bytes.NewBufferString(raw))
	if e != nil {
		t.Fatal(e)
	}
	wire, e := proto.Marshal(&types.ClientRegistration{ClientId: "c", Models: models})
	if e != nil {
		t.Fatal(e)
	}
	var got types.ClientRegistration
	if e = proto.Unmarshal(wire, &got); e != nil {
		t.Fatal(e)
	}
	want, _ := decodeJSONObject([]byte(`{"id":"m","object":"model","created":123,"owned_by":"vllm","root":"/weights/model","parent":null,"max_model_len":262144,"large_integer":9007199254740993,"capabilities":{"vision":true,"disabled":false,"list":["a",null,{"x":1}]}}`))
	actual, e := decodeJSONObject(got.Models[0].GetRawJson())
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(want, actual) {
		t.Fatalf("metadata changed: %#v", actual)
	}
}

func TestMetadataOnlyChangesTriggerModelUpdate(t *testing.T) {
	old, e := decodeModelInfo([]byte(`{"id":"m","root":"old","max_model_len":262144,"flags":{"vision":false}}`))
	if e != nil {
		t.Fatal(e)
	}
	c := &Client{models: []*types.ModelInfo{old}}
	for _, raw := range []string{`{"id":"m","root":"new","max_model_len":262144,"flags":{"vision":false}}`, `{"id":"m","root":"old","max_model_len":1048576,"flags":{"vision":false}}`, `{"id":"m","root":"old","max_model_len":262144,"flags":{"vision":true}}`} {
		updated, e := decodeModelInfo([]byte(raw))
		if e != nil {
			t.Fatal(e)
		}
		if c.modelsEqual([]*types.ModelInfo{updated}) {
			t.Fatalf("metadata update ignored: %s", raw)
		}
	}
	reordered, e := decodeModelInfo([]byte(`{"flags":{"vision":false},"max_model_len":262144,"root":"old","id":"m"}`))
	if e != nil {
		t.Fatal(e)
	}
	if !c.modelsEqual([]*types.ModelInfo{reordered}) {
		t.Fatal("JSON key order caused spurious update")
	}
}

func TestSyncModelsForwardsMetadataChangesWithoutReconnect(t *testing.T) {
	var polls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		n := polls.Add(1)
		fmt.Fprintf(w, `{"data":[{"id":"m","root":"new","max_model_len":1048576,"capabilities":{"vision":true},"created":%d,"permission":[{"id":"p%d","created":%d}]}]}`, n, n, n)
	}))
	defer upstream.Close()
	messages := make(chan *types.ClientMessage, 4)
	wsURL, closeWS := startClientMessageCaptureServer(t, messages)
	defer closeWS()
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	old, err := decodeModelInfo([]byte(`{"id":"m","root":"old","max_model_len":262144,"capabilities":{"vision":false},"created":0,"permission":[{"id":"p0","created":0}]}`))
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{BaseUrl: upstream.URL, models: []*types.ModelInfo{old}, conn: conn}
	c.syncModels()
	select {
	case msg := <-messages:
		t.Fatalf("metadata must settle for one poll before notification: %v", msg)
	default:
	}
	c.syncModels()
	select {
	case msg := <-messages:
		update := msg.GetModelUpdate()
		if update == nil || len(update.GetModels()) != 1 {
			t.Fatalf("expected one model update, got %v", msg)
		}
		payload, err := decodeJSONObject(update.GetModels()[0].GetRawJson())
		if err != nil {
			t.Fatal(err)
		}
		want, _ := decodeJSONObject([]byte(`{"id":"m","root":"new","max_model_len":1048576,"capabilities":{"vision":true},"created":2,"permission":[{"id":"p2","created":2}]}`))
		if !reflect.DeepEqual(want, payload) {
			t.Fatalf("metadata changed: %#v", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("metadata-only update was not sent over the existing connection")
	}
	c.syncModels()
	select {
	case msg := <-messages:
		t.Fatalf("unchanged metadata caused another update: %v", msg)
	case <-time.After(100 * time.Millisecond):
	}
}
