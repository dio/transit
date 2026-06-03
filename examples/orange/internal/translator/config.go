package translator

// ProviderConfig carries all static, per-upstream configuration that a
// Translator needs at construction time. Dynamic per-request data (e.g., the
// actual model name) is extracted from the request body inside RequestBody.
type ProviderConfig struct {
	// BackendSchema identifies the translator to instantiate.
	// Registry key: "openai", "anthropic", "awsbedrock", "awsanthropic",
	// "azureopenai", "gcpvertexai", "gcpanthropic".
	BackendSchema string

	// PathPrefix is the upstream base path, e.g. "/v1" for OpenAI,
	// "/openai/deployments" for Azure. Set to "" to use the provider default.
	PathPrefix string

	// BackendModel is the resolved backend model name to send to the upstream.
	// Set from the model entry's name: field (resolved by match). Empty means
	// the client model name passes through unchanged.
	BackendModel string

	// Extra holds provider-specific knobs.
	// Common keys:
	//   "azure_api_version"     — Azure OpenAI api-version query param
	//   "anthropic_version"     — anthropic-version header value (GCP/AWS)
	//   "aws_region"            — AWS region for SigV4 signing
	//   "gcp_project_id"        — GCP project ID for Vertex AI paths
	//   "gcp_location"          — GCP region, e.g. "us-central1"
	Extra map[string]string
}
