package iam

// IAMServiceProvider is the interface for IAM operations used by the gateway.
// Both local IAMService and RemoteIAMService implement this interface.
type IAMServiceProvider interface {
	// GetUserByAccessKeyID looks up a user by their access key ID
	GetUserByAccessKeyID(accessKeyID string) (*IAMUser, *AccessKey, error)

	// DecryptSecretKey decrypts an encrypted secret key
	DecryptSecretKey(encryptedSecret []byte) (string, error)

	// GetTempCredentialByAccessKeyID retrieves a temp credential by access key ID
	GetTempCredentialByAccessKeyID(accessKeyID string) (*TemporaryCredential, error)

	// GetUser gets a user by name
	GetUser(name string) (*IAMUser, error)

	// EvaluateAccess evaluates whether a request is allowed
	EvaluateAccess(ctx *EvalContext) *EvalResult
}

// Compile-time check that IAMService implements IAMServiceProvider
var _ IAMServiceProvider = (*IAMService)(nil)
