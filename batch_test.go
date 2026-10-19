package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestBatchAnswersOneLinePerFile(t *testing.T) {
	var in, out bytes.Buffer
	enc := json.NewEncoder(&in)
	for _, p := range []string{"a.ext", "b.ext"} {
		if err := enc.Encode(batchRequest{Path: p, Content: []byte("x")}); err != nil {
			t.Fatal(err)
		}
	}
	if err := serveBatch(&in, &out); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(&out)
	var ready map[string]string
	if err := dec.Decode(&ready); err != nil || ready["ready"] != batchProtocol {
		t.Fatalf("%v %v", ready, err)
	}
	for i := 0; i < 2; i++ {
		var resp batchResponse
		if err := dec.Decode(&resp); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
}
