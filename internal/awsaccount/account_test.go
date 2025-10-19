package awsaccount

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const successCallerIdentityResponse = `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
  <GetCallerIdentityResult>
    <Account>123456789012</Account>
    <Arn>arn:aws:iam::123456789012:user/test</Arn>
    <UserId>ABCDEF1234567890:test</UserId>
  </GetCallerIdentityResult>
  <ResponseMetadata>
    <RequestId>test-request</RequestId>
  </ResponseMetadata>
</GetCallerIdentityResponse>`

func TestCallerAccountIDSuccess(t *testing.T) {
	ctx := context.Background()

	formRequests := make(chan url.Values, 1)
	server := newSTSServer(t, http.StatusOK, successCallerIdentityResponse, formRequests)
	t.Cleanup(server.Close)

	setupAWSEnv(t, server.URL)

	account, err := CallerAccountID(ctx, "eu-west-1")
	if err != nil {
		t.Fatalf("CallerAccountID returned error: %v", err)
	}
	if want := "123456789012"; account != want {
		t.Fatalf("expected account %s, got %s", want, account)
	}

	select {
	case form := <-formRequests:
		if form.Get("Action") != "GetCallerIdentity" {
			t.Fatalf("expected Action=GetCallerIdentity, got %s", form.Get("Action"))
		}
		if form.Get("Version") != "2011-06-15" {
			t.Fatalf("expected Version=2011-06-15, got %s", form.Get("Version"))
		}
	default:
		t.Fatalf("expected request to STS, received none")
	}
}

func TestCallerAccountIDDefaultsRegion(t *testing.T) {
	ctx := context.Background()

	server := newSTSServer(t, http.StatusOK, successCallerIdentityResponse, nil)
	t.Cleanup(server.Close)

	setupAWSEnv(t, server.URL)
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")

	account, err := CallerAccountID(ctx, "")
	if err != nil {
		t.Fatalf("CallerAccountID returned error: %v", err)
	}
	if want := "123456789012"; account != want {
		t.Fatalf("expected account %s, got %s", want, account)
	}
}

func TestCallerAccountIDPropagatesSTSError(t *testing.T) {
	ctx := context.Background()

	server := newSTSServer(t, http.StatusForbidden, `<ErrorResponse>
  <Error>
    <Code>AccessDenied</Code>
    <Message>not allowed</Message>
  </Error>
</ErrorResponse>`, nil)
	t.Cleanup(server.Close)

	setupAWSEnv(t, server.URL)

	_, err := CallerAccountID(ctx, "eu-west-1")
	if err == nil {
		t.Fatal("expected error from CallerAccountID, got nil")
	}
	if !strings.Contains(err.Error(), "get caller identity") {
		t.Fatalf("expected error to mention GetCallerIdentity, got %v", err)
	}
}

func TestCallerAccountIDErrorsOnMissingAccount(t *testing.T) {
	ctx := context.Background()

	server := newSTSServer(t, http.StatusOK, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
  <GetCallerIdentityResult>
    <Arn>arn:aws:iam::123456789012:user/test</Arn>
    <UserId>ABCDEF1234567890:test</UserId>
  </GetCallerIdentityResult>
</GetCallerIdentityResponse>`, nil)
	t.Cleanup(server.Close)

	setupAWSEnv(t, server.URL)

	_, err := CallerAccountID(ctx, "eu-west-1")
	if err == nil {
		t.Fatal("expected error from CallerAccountID, got nil")
	}
	if !strings.Contains(err.Error(), "returned empty account") {
		t.Fatalf("expected missing account error, got %v", err)
	}
}

func setupAWSEnv(t *testing.T, endpoint string) {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret-key")
	t.Setenv("AWS_SESSION_TOKEN", "test-session-token")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_ENDPOINT_URL_STS", endpoint)
}

func newSTSServer(t *testing.T, status int, body string, requests chan<- url.Values) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests != nil {
			if err := r.ParseForm(); err != nil {
				t.Fatalf("failed to parse STS request form: %v", err)
			}
			values := url.Values{}
			for key, vals := range r.Form {
				values[key] = append(values[key], vals...)
			}
			select {
			case requests <- values:
			default:
			}
		}
		w.WriteHeader(status)
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatalf("failed to write STS response: %v", err)
		}
	}))
}
