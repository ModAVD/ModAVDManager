package update

import (
	"net/http"
	"reflect"
	"testing"

	"modavd/internal/hostproxy"
)

// The update finds the proxy where components fetch does: the Windows settings too, not only the environment.
func TestClientProxy(t *testing.T) {
	tr, ok := (&Updater{}).client().Transport.(*http.Transport)
	if !ok || reflect.ValueOf(tr.Proxy).Pointer() != reflect.ValueOf(hostproxy.ForRequest).Pointer() {
		t.Error("the update client does not use hostproxy.ForRequest")
	}
}
