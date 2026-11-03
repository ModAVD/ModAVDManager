package components

import (
	"net/http"
	"reflect"
	"testing"

	"modavd/internal/hostproxy"
)

func TestClientProxy(t *testing.T) {
	tr, ok := (&fetcher{}).httpClient().Transport.(*http.Transport)
	if !ok || reflect.ValueOf(tr.Proxy).Pointer() != reflect.ValueOf(hostproxy.ForRequest).Pointer() {
		t.Error("the download client does not use hostproxy.ForRequest")
	}
}
