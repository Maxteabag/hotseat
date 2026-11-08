package quota

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestUsageErrorKeepsNativeHTTPStatusWithoutChangingLegacyMessage(t *testing.T) {
	for _, status := range []int{401, 403, 404, 429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			client := clientFor(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
			_, err := FetchWith(context.Background(), client, "SYNTHETIC")
			var typed *UsageError
			if !errors.As(err, &typed) || typed.HTTPStatus != status {
				t.Fatal("lost native HTTP provenance")
			}
			if status == 401 || status == 403 {
				if err.Error() != "the account rejected this token" {
					t.Fatal("legacy message changed")
				}
			}
		})
	}
}

func TestTransportTimeoutAndCancellationRemainUnwrappableWithoutAuthGuessing(t *testing.T) {
	for _, cause := range []error{context.DeadlineExceeded, context.Canceled} {
		client := &http.Client{Transport: errTransport{cause}}
		_, usage := FetchWith(context.Background(), client, "SYNTHETIC")
		_, probe := ProbeWith(context.Background(), client, "SYNTHETIC")
		for _, err := range []error{usage, probe} {
			if !errors.Is(err, cause) {
				t.Fatal("native cause was reduced to text")
			}
		}
		var typedUsage *UsageError
		var typedProbe *ProbeError
		if !errors.As(usage, &typedUsage) || !errors.As(probe, &typedProbe) ||
			typedUsage.HTTPStatus != 0 || typedProbe.HTTPStatus != 0 {
			t.Fatal("transport error acquired a fabricated HTTP status")
		}
	}
}

func TestHeaderProbeRetainsItsOwnCredentialRejectionStatus(t *testing.T) {
	for _, status := range []int{401, 403} {
		calls := 0
		client := clientFor(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(status) })
		_, err := ProbeWith(context.Background(), client, "SYNTHETIC")
		var typed *ProbeError
		if !errors.As(err, &typed) || typed.HTTPStatus != status || calls != 1 ||
			err.Error() != "the account rejected this token" {
			t.Fatal("header probe provenance/message/order changed")
		}
	}
}
