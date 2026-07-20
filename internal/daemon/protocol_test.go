package daemon

import (
	"bytes"
	"encoding/json"
	"net"
	"reflect"
	"testing"
)

func TestProtocolRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value any
		into  any
	}{
		{
			name:  "request",
			value: Request{Op: "cluster.start", Args: json.RawMessage(`{"name":"demo"}`)},
			into:  &Request{},
		},
		{
			name:  "success response",
			value: Response{OK: true, Data: json.RawMessage(`{"pong":true}`)},
			into:  &Response{},
		},
		{
			name:  "error response",
			value: Response{OK: false, Error: "not found"},
			into:  &Response{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var wire bytes.Buffer
			if err := json.NewEncoder(&wire).Encode(test.value); err != nil {
				t.Fatal(err)
			}
			if wire.Len() == 0 || wire.Bytes()[wire.Len()-1] != '\n' {
				t.Fatalf("encoded message is not newline-delimited: %q", wire.Bytes())
			}
			if err := json.NewDecoder(&wire).Decode(test.into); err != nil {
				t.Fatal(err)
			}
			got := reflect.ValueOf(test.into).Elem().Interface()
			if !reflect.DeepEqual(got, test.value) {
				t.Fatalf("round trip = %#v, want %#v", got, test.value)
			}
		})
	}
}

func TestServeConnectionRoundTrip(t *testing.T) {
	t.Parallel()

	client, server := net.Pipe()
	done := make(chan struct{})
	service := &Server{}
	go func() {
		service.serveConnection(server)
		close(done)
	}()

	request := Request{Op: "daemon.ping", Args: json.RawMessage(`{}`)}
	if err := json.NewEncoder(client).Encode(request); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := json.NewDecoder(client).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || string(response.Data) != `{"pong":true}` {
		t.Fatalf("response = %#v", response)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
}
