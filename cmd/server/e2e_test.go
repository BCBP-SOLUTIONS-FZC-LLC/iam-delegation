//go:build e2e

// End-to-end tests (LLD §17.4) — boot the real composition root (run, this
// package's own entry point) against real Postgres, real Valkey, and a
// real SNS/SQS-compatible emulator (floci, matching docker-compose.yml's
// image), then drive it over real HTTP/SQS exactly as a caller/Core would.
// User Profile and Org Membership are faked with local httptest.Servers —
// those are this service's own outbound dependencies, not infrastructure
// this repo owns, and are already covered by unit tests against the real
// client contracts (internal/adapter/outbound/userprofile,.../orgmembership).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/postgres"
	gincommon "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	pgcommon "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// ── test logger ─────────────────────────────────────────────────────────

// e2eLogger is a trivial stdout port.Logger — real structured logging is
// unit-tested elsewhere; this suite only needs run() to have something
// non-nil to call (the Zap sink from logger.NewLogger).
type e2eLogger struct{ mu *sync.Mutex }

func newE2ELogger() e2eLogger { return e2eLogger{mu: &sync.Mutex{}} }

func (l e2eLogger) log(level, msg string, fields map[string]interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Printf("[e2e %s] %s %v\n", level, msg, fields)
}
func (l e2eLogger) Debug(msg string, fields map[string]interface{}) { l.log("DEBUG", msg, fields) }
func (l e2eLogger) Info(msg string, fields map[string]interface{})  { l.log("INFO", msg, fields) }
func (l e2eLogger) Warn(msg string, fields map[string]interface{})  { l.log("WARN", msg, fields) }
func (l e2eLogger) Error(msg string, fields map[string]interface{}) { l.log("ERROR", msg, fields) }

// ── Postgres fixture ────────────────────────────────────────────────────

type e2ePostgres struct {
	container testcontainers.Container
	superDSN  string
	appDSN    string
	sys       *pgcommon.Pool // superuser pool, used for direct seed/mutate SQL
}

const e2eAppPassword = "delegation_app_dev_password" // matches migrations/000001_schema.up.sql

func startE2EPostgres(ctx context.Context, t *testing.T) *e2ePostgres {
	t.Helper()
	req := testcontainers.ContainerRequest{
		Image:        "postgres:16-alpine",
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_DB":       "delegation",
			"POSTGRES_USER":     "postgres",
			"POSTGRES_PASSWORD": "postgres",
		},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(120 * time.Second),
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: req, Started: true})
	require.NoError(t, err)
	//nolint:contextcheck // cleanup must terminate the container even if ctx is already done
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	host, err := container.Host(ctx)
	require.NoError(t, err)
	port, err := container.MappedPort(ctx, "5432")
	require.NoError(t, err)

	superDSN := fmt.Sprintf("postgres://postgres:postgres@%s:%s/delegation?sslmode=disable", host, port.Port())
	appDSN := fmt.Sprintf("postgres://delegation_app:%s@%s:%s/delegation?sslmode=disable", e2eAppPassword, host, port.Port())

	// Same migration entry point cmd/server/main.go calls in run() — this
	// fixture can never drift from what actually ships.
	require.NoError(t, pgadapter.Migrate(ctx, superDSN))

	sysPool, err := pgcommon.NewPool(ctx, pgcommon.Config{DSN: superDSN})
	require.NoError(t, err)
	t.Cleanup(sysPool.Close)

	return &e2ePostgres{container: container, superDSN: superDSN, appDSN: appDSN, sys: sysPool}
}

func (p *e2ePostgres) exec(ctx context.Context, t *testing.T, sql string, args ...any) {
	t.Helper()
	err := p.sys.WithConn(ctx, func(ctx context.Context, conn *pgxpool.Conn) error {
		_, err := conn.Exec(ctx, sql, args...)
		return err
	})
	require.NoError(t, err)
}

func (p *e2ePostgres) queryRow(ctx context.Context, t *testing.T, sql string, args []any, dest ...any) {
	t.Helper()
	err := p.sys.WithConn(ctx, func(ctx context.Context, conn *pgxpool.Conn) error {
		return conn.QueryRow(ctx, sql, args...).Scan(dest...)
	})
	require.NoError(t, err)
}

// seedDelegation inserts a row directly (bypassing the public API's
// validation — needed for past-ends_at / manipulated review_due_at
// scenarios the API deliberately rejects). Unset fields get the LLD §7.2.1
// defaults a real create would produce.
type seedDelegation struct {
	ID                     uuid.UUID
	TenantID               uuid.UUID
	DelegatorID            uuid.UUID
	DelegateID             uuid.UUID
	Scope                  string
	StartsAt               time.Time
	EndsAt                 *time.Time
	Status                 string
	ReviewDueAt            *time.Time
	ReviewLastWarnedBucket *int
	ReviewWindowDays       *int
}

func (p *e2ePostgres) seed(ctx context.Context, t *testing.T, d seedDelegation) {
	t.Helper()
	if d.Scope == "" {
		d.Scope = "all"
	}
	if d.Status == "" {
		d.Status = "active"
	}
	p.exec(ctx, t, `
		INSERT INTO public.delegations
			(id, tenant_id, delegator_id, delegate_id,
			 delegator_membership_id, delegate_membership_id,
			 scope, starts_at, ends_at, status,
			 review_due_at, review_last_warned_bucket, review_window_days)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		d.ID, d.TenantID, d.DelegatorID, d.DelegateID,
		uuid.New(), uuid.New(),
		d.Scope, d.StartsAt, d.EndsAt, d.Status,
		d.ReviewDueAt, d.ReviewLastWarnedBucket, d.ReviewWindowDays,
	)
}

// ── Valkey fixture ──────────────────────────────────────────────────────

func startE2EValkey(ctx context.Context, t *testing.T) string {
	t.Helper()
	req := testcontainers.ContainerRequest{
		Image:        "valkey/valkey:8-alpine",
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor:   wait.ForListeningPort("6379/tcp").WithStartupTimeout(60 * time.Second),
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: req, Started: true})
	require.NoError(t, err)
	//nolint:contextcheck // cleanup must terminate the container even if ctx is already done
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	host, err := container.Host(ctx)
	require.NoError(t, err)
	port, err := container.MappedPort(ctx, "6379")
	require.NoError(t, err)
	return net.JoinHostPort(host, port.Port())
}

// ── Floci (SNS/SQS-compatible) fixture ─────────────────────────────────

type e2ebroker struct {
	endpoint             string
	sns                  *sns.Client
	sqs                  *sqs.Client
	topicARN             string
	cascadeQueueURL      string
	workflowQueueURL     string
	notificationQueueURL string
	auditQueueURL        string
}

func startE2EBroker(ctx context.Context, t *testing.T) *e2ebroker {
	t.Helper()
	req := testcontainers.ContainerRequest{
		Image:        "floci/floci:2.1.0-compat",
		ExposedPorts: []string{"4566/tcp"},
		Env: map[string]string{
			"FLOCI_DEFAULT_REGION":     "ap-south-1",
			"FLOCI_DEFAULT_ACCOUNT_ID": "000000000000",
		},
		WaitingFor: wait.ForListeningPort("4566/tcp").WithStartupTimeout(120 * time.Second),
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: req, Started: true})
	require.NoError(t, err)
	//nolint:contextcheck // cleanup must terminate the container even if ctx is already done
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	host, err := container.Host(ctx)
	require.NoError(t, err)
	port, err := container.MappedPort(ctx, "4566")
	require.NoError(t, err)
	endpoint := fmt.Sprintf("http://%s:%s", host, port.Port())

	// Explicit static credentials — do not rely on AWS_ACCESS_KEY_ID/
	// AWS_SECRET_ACCESS_KEY env vars being set yet: this fixture runs
	// before TestE2E's t.Setenv calls.
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("ap-south-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	require.NoError(t, err)
	snsClient := sns.NewFromConfig(awsCfg, func(o *sns.Options) { o.BaseEndpoint = &endpoint })
	sqsClient := sqs.NewFromConfig(awsCfg, func(o *sqs.Options) { o.BaseEndpoint = &endpoint })

	// Floci needs a moment past "listening" before it accepts SNS/SQS API
	// calls — poll ListTopics until it succeeds instead of a fixed sleep.
	require.Eventually(t, func() bool {
		_, err := snsClient.ListTopics(ctx, &sns.ListTopicsInput{})
		return err == nil
	}, 60*time.Second, 500*time.Millisecond, "floci did not become ready")

	b := &e2ebroker{endpoint: endpoint, sns: snsClient, sqs: sqsClient}
	b.provision(ctx, t)
	return b
}

// provision replicates scripts/init-floci.sh's SNS topic + inbound queue +
// three downstream fan-out subscriber queues (Workflow/Notification/Audit —
// the tender queue was removed from that script earlier this session and
// is deliberately not reproduced here), via the AWS SDK directly rather
// than shelling out to the script.
func (b *e2ebroker) provision(ctx context.Context, t *testing.T) {
	t.Helper()

	topic, err := b.sns.CreateTopic(ctx, &sns.CreateTopicInput{Name: strPtr("iam-delegation-events")})
	require.NoError(t, err)
	b.topicARN = *topic.TopicArn

	b.cascadeQueueURL = b.createQueue(ctx, t, "delegation-cascade-q")
	b.workflowQueueURL = b.createSubscriber(ctx, t, "delegation-workflow-q",
		`{"EventType":["DelegationStarted","DelegationEnded","DelegationEscalationRequested"]}`)
	b.notificationQueueURL = b.createSubscriber(ctx, t, "delegation-notification-q",
		`{"EventType":["DelegationStarted","DelegationEnded","DelegationReviewRequested","DelegationEscalationRequested"]}`)
	b.auditQueueURL = b.createSubscriber(ctx, t, "delegation-audit-q", "")
}

func (b *e2ebroker) createQueue(ctx context.Context, t *testing.T, name string) string {
	t.Helper()
	out, err := b.sqs.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: strPtr(name)})
	require.NoError(t, err)
	return *out.QueueUrl
}

func (b *e2ebroker) queueARN(ctx context.Context, t *testing.T, queueURL string) string {
	t.Helper()
	out, err := b.sqs.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       &queueURL,
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn},
	})
	require.NoError(t, err)
	return out.Attributes[string(sqstypes.QueueAttributeNameQueueArn)]
}

func (b *e2ebroker) createSubscriber(ctx context.Context, t *testing.T, name, filterPolicy string) string {
	t.Helper()
	queueURL := b.createQueue(ctx, t, name)
	queueARN := b.queueARN(ctx, t, queueURL)

	sub, err := b.sns.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: &b.topicARN,
		Protocol: strPtr("sqs"),
		Endpoint: &queueARN,
		Attributes: map[string]string{
			"RawMessageDelivery": "true",
		},
	})
	require.NoError(t, err)

	if filterPolicy != "" {
		_, err = b.sns.SetSubscriptionAttributes(ctx, &sns.SetSubscriptionAttributesInput{
			SubscriptionArn: sub.SubscriptionArn,
			AttributeName:   strPtr("FilterPolicy"),
			AttributeValue:  &filterPolicy,
		})
		require.NoError(t, err)
	}
	return queueURL
}

func strPtr(s string) *string { return &s }

// ── SQS message polling helpers ─────────────────────────────────────────

// envEnvelope mirrors events.Envelope[json.RawMessage]'s wire shape (see
// platform-events/pkg/events.Envelope) — decoded locally so this test file
// doesn't need to import the library's generic type.
type envEnvelope struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Source   string          `json:"source"`
	TenantID string          `json:"tenant_id"`
	Subject  string          `json:"subject"`
	Time     time.Time       `json:"time"`
	Data     json.RawMessage `json:"data"`
}

// receiveMatching polls queueURL until a message whose decoded envelope
// satisfies match arrives, deleting every message it receives (matched or
// not) so later assertions in the same suite never see stale messages.
// Fails the test if nothing matches within timeout.
func receiveMatching(ctx context.Context, t *testing.T, sqsClient *sqs.Client, queueURL string, timeout time.Duration, match func(envEnvelope) bool) envEnvelope {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := sqsClient.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            &queueURL,
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     2,
			VisibilityTimeout:   5,
		})
		require.NoError(t, err)
		for _, m := range out.Messages {
			_, _ = sqsClient.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: &queueURL, ReceiptHandle: m.ReceiptHandle})
			var env envEnvelope
			if err := json.Unmarshal([]byte(*m.Body), &env); err != nil {
				continue
			}
			if match(env) {
				return env
			}
		}
	}
	t.Fatalf("no matching message arrived on %s within %s", queueURL, timeout)
	return envEnvelope{}
}

// assertNoMatch drains queueURL for window and fails the test if any
// message satisfying match arrives — used to assert the DLG-EVT-4 silent
// (no-event) delegator-side cascade end.
func assertNoMatch(ctx context.Context, t *testing.T, sqsClient *sqs.Client, queueURL string, window time.Duration, match func(envEnvelope) bool) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		out, err := sqsClient.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            &queueURL,
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     2,
			VisibilityTimeout:   5,
		})
		require.NoError(t, err)
		for _, m := range out.Messages {
			_, _ = sqsClient.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: &queueURL, ReceiptHandle: m.ReceiptHandle})
			var env envEnvelope
			if err := json.Unmarshal([]byte(*m.Body), &env); err != nil {
				continue
			}
			require.Falsef(t, match(env), "unexpected event landed on %s: type=%s subject=%s", queueURL, env.Type, env.Subject)
		}
	}
}

// ── Fake User Profile / Org Membership ─────────────────────────────────

type availabilityCall struct {
	UserID string
	Body   map[string]any
}

type fakeUserProfile struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []availabilityCall
}

func newFakeUserProfile() *fakeUserProfile {
	f := &fakeUserProfile{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		body, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)
		// Paths handled:
		//   PUT    /api/v1/internal/users/{userID}/availability
		//   DELETE /api/v1/internal/users/{userID}/availability/delegate
		//   GET    /api/v1/users/{userID}/availability
		// The user ID is always the segment immediately after "users/".
		// Using parts[len-2] was wrong for the DELETE path: it returned
		// "availability" instead of the UUID (Gap 3 fix added the /delegate suffix).
		parts := strings.Split(r.URL.Path, "/")
		userID := ""
		for i, p := range parts {
			if p == "users" && i+1 < len(parts) {
				userID = parts[i+1]
				break
			}
		}
		f.mu.Lock()
		f.calls = append(f.calls, availabilityCall{UserID: userID, Body: decoded})
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	return f
}

func (f *fakeUserProfile) lastCallFor(userID string) (availabilityCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].UserID == userID {
			return f.calls[i], true
		}
	}
	return availabilityCall{}, false
}

func newFakeOrgMembership() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"active":true,"tenant_membership_id":%q}`, uuid.New().String())
	}))
}

// ── HTTP client helpers ──────────────────────────────────────────────────

type e2eClient struct {
	baseURL string
	http    *http.Client
}

// do returns the response status code and decoded JSON body — never the
// *http.Response itself, so callers can't forget to close its body (this
// method already does).
func (c *e2eClient) do(ctx context.Context, t *testing.T, method, path string, tenantID, userID uuid.UUID, roles string, body any) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(gincommon.HeaderTenantID, tenantID.String())
	req.Header.Set(gincommon.HeaderUserID, userID.String())
	if roles != "" {
		req.Header.Set(gincommon.HeaderTenantRoles, roles)
	}
	if method == http.MethodPost {
		// requireIdempotencyKey() gates POST /api/v1/delegations (DLG-2) —
		// harmless to set on every POST, since no other route checks it.
		req.Header.Set("Idempotency-Key", uuid.New().String())
	}
	resp, err := c.http.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, decoded
}

// ── free port helper ─────────────────────────────────────────────────────

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// ── TestE2E ───────────────────────────────────────────────────────────────

func TestE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	pg := startE2EPostgres(ctx, t)
	valkeyAddr := startE2EValkey(ctx, t)
	broker := startE2EBroker(ctx, t)

	up := newFakeUserProfile()
	defer up.srv.Close()
	org := newFakeOrgMembership()
	defer org.Close()

	httpPort := freePort(t)
	metricsPort := freePort(t)

	t.Setenv("ENVIRONMENT", "test")
	t.Setenv("PORT", strconv.Itoa(httpPort))
	t.Setenv("METRICS_PORT", strconv.Itoa(metricsPort))
	t.Setenv("DATABASE_URL", pg.appDSN)
	t.Setenv("MIGRATION_DATABASE_URL", pg.superDSN)
	t.Setenv("SYSTEM_DATABASE_URL", pg.superDSN)
	t.Setenv("VALKEY_ADDR", valkeyAddr)
	t.Setenv("USER_PROFILE_BASE_URL", up.srv.URL)
	t.Setenv("ORG_MEMBERSHIP_BASE_URL", org.URL)
	t.Setenv("AWS_REGION", "ap-south-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_ENDPOINT_URL", broker.endpoint)
	t.Setenv("SNS_TOPIC_ARN", broker.topicARN)
	t.Setenv("CASCADE_QUEUE_URL", broker.cascadeQueueURL)
	t.Setenv("GLUE_REGISTRY_NAME", "")
	t.Setenv("DOCS_ENABLED", "false")
	t.Setenv("DOCS_AUTH_TOKEN", "")

	runCtx, runCancel := context.WithCancel(context.Background())
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- run(runCtx, newE2ELogger()) }()

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", httpPort)
	waitHealthy(t, baseURL)

	t.Cleanup(func() {
		runCancel()
		select {
		case err := <-runErrCh:
			require.NoError(t, err)
		case <-time.After(30 * time.Second):
			t.Fatal("server did not shut down within 30s")
		}
	})

	client := &e2eClient{baseURL: baseURL, http: &http.Client{Timeout: 15 * time.Second}}

	t.Run("CreateCancelFanout", func(t *testing.T) {
		testCreateCancelFanout(ctx, t, pg, broker, up, client)
	})
	t.Run("Expiry", func(t *testing.T) {
		testExpiry(ctx, t, pg, broker, up, client)
	})
	t.Run("ReviewCascade", func(t *testing.T) {
		testReviewCascade(ctx, t, pg, broker, client)
	})
	t.Run("UserRemovalCascade", func(t *testing.T) {
		testUserRemovalCascade(ctx, t, pg, broker)
	})
}

func waitHealthy(t *testing.T, baseURL string) {
	t.Helper()
	require.Eventually(t, func() bool {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, baseURL+"/healthz", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode == http.StatusOK
	}, 30*time.Second, 200*time.Millisecond, "server did not become healthy")
}

// ── Scenario 1: create -> fan-out -> cancel -> fan-out (restore) ────────

func testCreateCancelFanout(ctx context.Context, t *testing.T, pg *e2ePostgres, broker *e2ebroker, up *fakeUserProfile, client *e2eClient) {
	tenantID, delegatorID, delegateID := uuid.New(), uuid.New(), uuid.New()

	status, body := client.do(ctx, t, http.MethodPost, "/api/v1/delegations", tenantID, delegatorID, "", map[string]any{
		"delegate_id": delegateID.String(),
		"scope":       "all",
	})
	_ = pg
	require.Equal(t, http.StatusCreated, status, "create response: %v", body)
	delegationID := body["delegation_id"].(string)
	recordVersion := body["record_version"].(float64)

	isStarted := func(env envEnvelope) bool { return env.Type == "DelegationStarted" && env.Subject == delegationID }
	receiveMatching(ctx, t, broker.sqs, broker.workflowQueueURL, 20*time.Second, isStarted)
	receiveMatching(ctx, t, broker.sqs, broker.notificationQueueURL, 20*time.Second, isStarted)

	require.Eventually(t, func() bool {
		_, ok := up.lastCallFor(delegatorID.String())
		return ok
	}, 10*time.Second, 100*time.Millisecond, "User Profile never received the availability-set call")
	setCall, _ := up.lastCallFor(delegatorID.String())
	require.Equal(t, delegateID.String(), setCall.Body["delegate_id"])

	status, body = client.do(ctx, t, http.MethodDelete, fmt.Sprintf("/api/v1/delegations/%s?record_version=%d", delegationID, int(recordVersion)), tenantID, delegatorID, "", nil)
	require.Equal(t, http.StatusOK, status, "cancel response: %v", body)
	require.Equal(t, "cancelled", body["status"])

	isEndedCancelled := func(env envEnvelope) bool {
		if env.Type != "DelegationEnded" || env.Subject != delegationID {
			return false
		}
		var data map[string]any
		_ = json.Unmarshal(env.Data, &data)
		return data["ended_reason"] == "cancelled"
	}
	receiveMatching(ctx, t, broker.sqs, broker.workflowQueueURL, 20*time.Second, isEndedCancelled)
	receiveMatching(ctx, t, broker.sqs, broker.notificationQueueURL, 20*time.Second, isEndedCancelled)

	require.Eventually(t, func() bool {
		call, ok := up.lastCallFor(delegatorID.String())
		return ok && call.Body["delegate_id"] == nil
	}, 10*time.Second, 100*time.Millisecond, "User Profile never received the pointer-clear call")
}

// ── Scenario 2: expiry ───────────────────────────────────────────────────

func testExpiry(ctx context.Context, t *testing.T, pg *e2ePostgres, broker *e2ebroker, up *fakeUserProfile, client *e2eClient) {
	tenantID, delegatorID, delegateID := uuid.New(), uuid.New(), uuid.New()
	delegationID := uuid.New()
	past := time.Now().Add(-48 * time.Hour)

	pg.seed(ctx, t, seedDelegation{
		ID: delegationID, TenantID: tenantID, DelegatorID: delegatorID, DelegateID: delegateID,
		StartsAt: past.Add(-24 * time.Hour), EndsAt: &past, Status: "active",
	})

	status, body := client.do(ctx, t, http.MethodPost, "/internal/delegations/expire", uuid.Nil, uuid.Nil, "", nil)
	require.Equal(t, http.StatusOK, status, "expire response: %v", body)
	require.GreaterOrEqual(t, int(body["succeeded"].(float64)), 1)

	var dbStatus string
	pg.queryRow(ctx, t, `SELECT status::text FROM public.delegations WHERE id = $1`, []any{delegationID}, &dbStatus)
	require.Equal(t, "ended", dbStatus)

	isEndedExpired := func(env envEnvelope) bool {
		if env.Type != "DelegationEnded" || env.Subject != delegationID.String() {
			return false
		}
		var data map[string]any
		_ = json.Unmarshal(env.Data, &data)
		return data["ended_reason"] == "expired"
	}
	receiveMatching(ctx, t, broker.sqs, broker.notificationQueueURL, 20*time.Second, isEndedExpired)

	require.Eventually(t, func() bool {
		call, ok := up.lastCallFor(delegatorID.String())
		return ok && call.Body["delegate_id"] == nil
	}, 10*time.Second, 100*time.Millisecond, "User Profile never received the expiry pointer-clear call")
}

// ── Scenario 3: review daily cascade ────────────────────────────────────

func testReviewCascade(ctx context.Context, t *testing.T, pg *e2ePostgres, broker *e2ebroker, client *e2eClient) {
	tenantID, delegatorID, delegateID := uuid.New(), uuid.New(), uuid.New()
	delegationID := uuid.New()
	now := time.Now()

	pg.seed(ctx, t, seedDelegation{
		ID: delegationID, TenantID: tenantID, DelegatorID: delegatorID, DelegateID: delegateID,
		StartsAt: now.Add(-30 * 24 * time.Hour), EndsAt: nil, Status: "active",
		ReviewDueAt: timePtr(now.Add(3 * 24 * time.Hour)),
	})

	sweep := func() map[string]any {
		status, body := client.do(ctx, t, http.MethodPost, "/internal/delegations/review-sweep", uuid.Nil, uuid.Nil, "", nil)
		require.Equal(t, http.StatusOK, status, "review-sweep response: %v", body)
		return body
	}
	isReviewRequested := func(days int) func(envEnvelope) bool {
		return func(env envEnvelope) bool {
			if env.Type != "DelegationReviewRequested" || env.Subject != delegationID.String() {
				return false
			}
			var data map[string]any
			_ = json.Unmarshal(env.Data, &data)
			dr, _ := data["days_remaining"].(float64)
			return int(dr) == days
		}
	}

	// day 3
	sweep()
	receiveMatching(ctx, t, broker.sqs, broker.notificationQueueURL, 20*time.Second, isReviewRequested(3))

	// day 2 — pull review_due_at 1 day closer and reset the warned bucket
	// so the sweep re-fires for the next bucket, mirroring what a real
	// day's passage would do.
	pg.exec(ctx, t, `UPDATE public.delegations SET review_due_at = $1, review_last_warned_bucket = NULL WHERE id = $2`,
		now.Add(2*24*time.Hour), delegationID)
	sweep()
	receiveMatching(ctx, t, broker.sqs, broker.notificationQueueURL, 20*time.Second, isReviewRequested(2))

	// day 1
	pg.exec(ctx, t, `UPDATE public.delegations SET review_due_at = $1, review_last_warned_bucket = NULL WHERE id = $2`,
		now.Add(1*24*time.Hour), delegationID)
	sweep()
	receiveMatching(ctx, t, broker.sqs, broker.notificationQueueURL, 20*time.Second, isReviewRequested(1))

	// past due -> auto-end review_expired
	pg.exec(ctx, t, `UPDATE public.delegations SET review_due_at = $1, review_last_warned_bucket = NULL WHERE id = $2`,
		now.Add(-1*time.Hour), delegationID)
	sweep()

	var dbStatus string
	pg.queryRow(ctx, t, `SELECT status::text FROM public.delegations WHERE id = $1`, []any{delegationID}, &dbStatus)
	require.Equal(t, "ended", dbStatus)

	isEndedReviewExpired := func(env envEnvelope) bool {
		if env.Type != "DelegationEnded" || env.Subject != delegationID.String() {
			return false
		}
		var data map[string]any
		_ = json.Unmarshal(env.Data, &data)
		return data["ended_reason"] == "review_expired"
	}
	receiveMatching(ctx, t, broker.sqs, broker.notificationQueueURL, 20*time.Second, isEndedReviewExpired)
}

// ── Scenario 4: user-removal cascade (delegate-side event, delegator-side silent) ─

func testUserRemovalCascade(ctx context.Context, t *testing.T, pg *e2ePostgres, broker *e2ebroker) {
	tenantID := uuid.New()
	removedUserID := uuid.New()
	otherDelegator, otherDelegate := uuid.New(), uuid.New()

	delegateSideID := uuid.New()  // removedUserID is the delegate here
	delegatorSideID := uuid.New() // removedUserID is the delegator here

	now := time.Now()
	pg.seed(ctx, t, seedDelegation{
		ID: delegateSideID, TenantID: tenantID, DelegatorID: otherDelegator, DelegateID: removedUserID,
		StartsAt: now.Add(-time.Hour), EndsAt: timePtr(now.Add(30 * 24 * time.Hour)), Status: "active",
	})
	pg.seed(ctx, t, seedDelegation{
		ID: delegatorSideID, TenantID: tenantID, DelegatorID: removedUserID, DelegateID: otherDelegate,
		StartsAt: now.Add(-time.Hour), EndsAt: timePtr(now.Add(30 * 24 * time.Hour)), Status: "active",
	})

	msg := fmt.Sprintf(`{
		"id":%q, "type":"MembershipRevoked", "source":"iam-org-membership",
		"specversion":"1.0", "tenant_id":%q, "time":%q,
		"data": {"tenant_id":%q, "user_id":%q, "actor_id":%q}
	}`, uuid.New().String(), tenantID.String(), now.UTC().Format(time.RFC3339),
		tenantID.String(), removedUserID.String(), uuid.New().String())

	_, err := broker.sqs.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    &broker.cascadeQueueURL,
		MessageBody: strPtr(msg),
	})
	require.NoError(t, err)

	// Delegate-side: ends and emits DelegationEnded{ended_reason=delegate_removed}.
	isEndedRemoved := func(env envEnvelope) bool {
		if env.Type != "DelegationEnded" || env.Subject != delegateSideID.String() {
			return false
		}
		var data map[string]any
		_ = json.Unmarshal(env.Data, &data)
		return data["ended_reason"] == "delegate_removed"
	}
	receiveMatching(ctx, t, broker.sqs, broker.notificationQueueURL, 30*time.Second, isEndedRemoved)

	var dbStatus string
	pg.queryRow(ctx, t, `SELECT status::text FROM public.delegations WHERE id = $1`, []any{delegateSideID}, &dbStatus)
	require.Equal(t, "ended", dbStatus)

	// Delegator-side: also ends, but DLG-EVT-4 says silently — no event for
	// this delegation_id lands on any fan-out queue.
	require.Eventually(t, func() bool {
		var s string
		pg.queryRow(ctx, t, `SELECT status::text FROM public.delegations WHERE id = $1`, []any{delegatorSideID}, &s)
		return s == "ended"
	}, 15*time.Second, 200*time.Millisecond, "delegator-side row never ended")

	matchesDelegatorSide := func(env envEnvelope) bool { return env.Subject == delegatorSideID.String() }
	assertNoMatch(ctx, t, broker.sqs, broker.notificationQueueURL, 5*time.Second, matchesDelegatorSide)
	assertNoMatch(ctx, t, broker.sqs, broker.workflowQueueURL, 5*time.Second, matchesDelegatorSide)
}

func timePtr(t time.Time) *time.Time { return &t }
