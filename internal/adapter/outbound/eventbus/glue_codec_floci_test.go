package eventbus

// Glue schema-version resolution against a real (floci) Glue Schema
// Registry. Verifies GlueCodec stamps events with the version matching this
// binary's embedded schema (GetSchemaByDefinition), not the registry's
// latest — the property the retired LatestVersion prefetch + 5-minute
// refresher got wrong during deploy/registration races and rollbacks.
// Skipped under -short or when Docker is unavailable, like the Postgres
// testcontainer suites.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	gluetypes "github.com/aws/aws-sdk-go-v2/service/glue/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/eventschema"
)

// flociImage matches docker-compose.yml's floci service.
const flociImage = "floci/floci:2.1.0"

var (
	flociOnce     sync.Once
	flociEndpoint string
	flociErr      error
)

func startFloci(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping floci integration test in short mode")
	}
	flociOnce.Do(func() {
		ctx := context.Background()
		c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				Image:        flociImage,
				ExposedPorts: []string{"4566/tcp"},
				Env:          map[string]string{"FLOCI_DEFAULT_REGION": "ap-south-1", "FLOCI_DEFAULT_ACCOUNT_ID": "000000000000"},
				WaitingFor:   wait.ForLog("Ready.").WithStartupTimeout(60 * time.Second),
			},
			Started: true,
		})
		if err != nil {
			flociErr = fmt.Errorf("start floci: %w", err)
			return
		}
		host, err := c.Host(ctx)
		if err != nil {
			flociErr = err
			return
		}
		port, err := c.MappedPort(ctx, "4566/tcp")
		if err != nil {
			flociErr = err
			return
		}
		flociEndpoint = fmt.Sprintf("http://%s:%s", host, port.Port())
	})
	if flociErr != nil {
		t.Skipf("skipping — floci unavailable (%v)", flociErr)
	}
	return flociEndpoint
}

func newFlociGlue(t *testing.T) *glue.Client {
	t.Helper()
	return glue.New(glue.Options{
		Region:       "ap-south-1",
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		BaseEndpoint: aws.String(startFloci(t)),
	})
}

// uploadedForm is an embedded schema exactly as schema-gov register uploads it.
func uploadedForm(t *testing.T, raw []byte) string {
	t.Helper()
	def, err := registeredDefinition(raw)
	require.NoError(t, err)
	return def
}

func createFlociRegistry(t *testing.T, cli *glue.Client) string {
	t.Helper()
	name := "it-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	_, err := cli.CreateRegistry(context.Background(), &glue.CreateRegistryInput{RegistryName: &name})
	require.NoError(t, err)
	return name
}

func createFlociSchema(t *testing.T, cli *glue.Client, registry, name, definition string) string {
	t.Helper()
	out, err := cli.CreateSchema(context.Background(), &glue.CreateSchemaInput{
		RegistryId:       &gluetypes.RegistryId{RegistryName: &registry},
		SchemaName:       &name,
		DataFormat:       gluetypes.DataFormatJson,
		Compatibility:    gluetypes.CompatibilityBackward,
		SchemaDefinition: &definition,
	})
	require.NoError(t, err)
	return aws.ToString(out.SchemaVersionId)
}

func stampedVersion(t *testing.T, codec *GlueCodec, schema string) string {
	t.Helper()
	_, vid, err := codec.Encode(context.Background(), schema, json.RawMessage(`{}`))
	require.NoError(t, err)
	return vid
}

// A binary keeps stamping the version it ships even after a newer version
// is registered — the rollback / registered-ahead-of-deploy case.
func TestGlueCodecFloci_ResolvesOwnVersion_NotLatest(t *testing.T) {
	cli := newFlociGlue(t)
	registry := createFlociRegistry(t, cli)
	schema := "DelegationStarted"

	v1 := createFlociSchema(t, cli, registry, schema, uploadedForm(t, eventschema.ByEventType[schema]))

	// A BACKWARD-compatible change (annotation only).
	var doc map[string]any
	require.NoError(t, json.Unmarshal(eventschema.ByEventType[schema], &doc))
	doc["description"] = "v2"
	v2Def, err := json.Marshal(doc)
	require.NoError(t, err)
	v2Out, err := cli.RegisterSchemaVersion(context.Background(), &glue.RegisterSchemaVersionInput{
		SchemaId:         &gluetypes.SchemaId{RegistryName: &registry, SchemaName: &schema},
		SchemaDefinition: aws.String(string(v2Def)),
	})
	require.NoError(t, err)
	v2 := aws.ToString(v2Out.SchemaVersionId)
	require.NotEqual(t, v1, v2)

	latest, err := cli.GetSchemaVersion(context.Background(), &glue.GetSchemaVersionInput{
		SchemaId:            &gluetypes.SchemaId{RegistryName: &registry, SchemaName: &schema},
		SchemaVersionNumber: &gluetypes.SchemaVersionNumber{LatestVersion: true},
	})
	require.NoError(t, err)
	require.Equal(t, v2, aws.ToString(latest.SchemaVersionId), "precondition: v2 is latest")

	codec, err := NewGlueCodec(context.Background(), cli, registry, []string{schema})
	require.NoError(t, err)
	assert.Equal(t, v1, stampedVersion(t, codec, schema),
		"must stamp the version matching the embedded (v1) schema, not latest (v2)")
}

// Every produced schema, registered exactly as CI registers it, resolves to
// its own version.
func TestGlueCodecFloci_ResolvesAllProducedSchemas(t *testing.T) {
	cli := newFlociGlue(t)
	registry := createFlociRegistry(t, cli)

	names := make([]string, 0, len(eventschema.ByEventType))
	want := map[string]string{}
	for name, raw := range eventschema.ByEventType {
		names = append(names, name)
		want[name] = createFlociSchema(t, cli, registry, name, uploadedForm(t, raw))
	}

	codec, err := NewGlueCodec(context.Background(), cli, registry, names)
	require.NoError(t, err)
	for name, vid := range want {
		assert.Equal(t, vid, stampedVersion(t, codec, name), name)
	}
}

// A deploy that outruns schema registration fails startup instead of
// stamping a wrong or missing version.
func TestGlueCodecFloci_UnregisteredDefinition_FailsStartup(t *testing.T) {
	cli := newFlociGlue(t)
	registry := createFlociRegistry(t, cli)

	// Registered, but with a different (older) definition than the binary's.
	createFlociSchema(t, cli, registry, "DelegationEnded", `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","additionalProperties":true}`)

	_, err := NewGlueCodec(context.Background(), cli, registry, []string{"DelegationEnded"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "isn't registered yet")

	// The snake_case name the old CI register step created is not what the
	// codec looks up.
	createFlociSchema(t, cli, registry, "delegation_started", uploadedForm(t, eventschema.ByEventType["DelegationStarted"]))
	_, err = NewGlueCodec(context.Background(), cli, registry, []string{"DelegationStarted"})
	require.Error(t, err, "schema absent under its PascalCase name")
}
