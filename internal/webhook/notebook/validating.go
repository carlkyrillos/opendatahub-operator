//go:build !nowebhook

package notebook

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/opendatahub-io/opendatahub-operator/v2/pkg/cluster/gvk"
	webhookutils "github.com/opendatahub-io/opendatahub-operator/v2/pkg/webhook"
)

//+kubebuilder:webhook:path=/platform-dataconnection-notebook,mutating=false,failurePolicy=fail,sideEffects=None,groups=kubeflow.org,resources=notebooks,verbs=create;update,versions=v1,name=notebook-connections-validator.opendatahub.io,admissionReviewVersions=v1
//nolint:lll

const (
	ConnectionsAnnotation = "opendatahub.io/connections"
)

// Validator implements webhook.AdmissionHandler for Notebook connection validation webhooks.
type Validator struct {
	Client  client.Client
	Decoder admission.Decoder
	Name    string
}

// Assert that Validator implements admission.Handler interface.
var _ admission.Handler = &Validator{}

// SetupWithManager registers the validating webhook with the provided controller-runtime manager.
//
// Parameters:
//   - mgr: The controller-runtime manager to register the webhook with.
//
// Returns:
//   - error: Always nil (for future extensibility).
func (v *Validator) SetupWithManager(mgr ctrl.Manager) error {
	hookServer := mgr.GetWebhookServer()
	hookServer.Register("/platform-dataconnection-notebook", &webhook.Admission{
		Handler:        v,
		LogConstructor: webhookutils.NewWebhookLogConstructor(v.Name),
	})
	return nil
}

// Handle processes admission requests for create and update operations on Notebook resources.
// It validates that the user has permission to access connection secrets listed in the
// opendatahub.io/connections annotation.
//
// Parameters:
//   - ctx: Context for the admission request (logger is extracted from here).
//   - req: The admission.Request containing the operation and object details.
//
// Returns:
//   - admission.Response: The result of the admission check, indicating whether the operation is allowed or denied.
func (v *Validator) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := logf.FromContext(ctx)

	// Check if decoder is properly injected
	if v.Decoder == nil {
		log.Error(nil, "Decoder is nil - webhook not properly initialized")
		return admission.Errored(http.StatusInternalServerError, errors.New("webhook decoder not initialized"))
	}

	// Validate that we're processing the correct Kind
	if req.Kind.Kind != gvk.Notebook.Kind {
		err := fmt.Errorf("unexpected kind: %s", req.Kind.Kind)
		log.Error(err, "got wrong kind", "group", req.Kind.Group, "version", req.Kind.Version, "kind", req.Kind.Kind)
		return admission.Errored(http.StatusBadRequest, err)
	}

	var resp admission.Response

	switch req.Operation {
	case admissionv1.Create, admissionv1.Update:
		resp = v.validateConnectionPermissions(ctx, &req)
	default:
		resp = admission.Allowed(fmt.Sprintf("Operation %s on %s allowed", req.Operation, req.Kind.Kind))
	}

	return resp
}

// validateConnectionPermissions validates that the user has permission to access connection secrets.
func (v *Validator) validateConnectionPermissions(ctx context.Context, req *admission.Request) admission.Response {
	log := logf.FromContext(ctx)

	// Decode the object from the request
	obj := &unstructured.Unstructured{}
	if err := v.Decoder.Decode(*req, obj); err != nil {
		log.Error(err, "failed to decode object")
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("failed to decode object: %w", err))
	}

	// Get the connections annotation
	annotations := obj.GetAnnotations()
	if annotations == nil {
		// No annotations - allow the request
		return admission.Allowed("No annotations found - connections validation skipped")
	}

	connectionsValue, found := annotations[ConnectionsAnnotation]
	if !found {
		// No connections annotation - allow the request
		return admission.Allowed("No connections annotation found - connections validation skipped")
	}

	if strings.TrimSpace(connectionsValue) == "" {
		// Empty connections annotation - allow the request
		return admission.Allowed("Empty connections annotation - connections validation skipped")
	}

	// Parse the connections annotation
	connectionSecrets, err := parseConnectionsAnnotation(connectionsValue)
	if err != nil {
		log.Error(err, "failed to parse connections annotation", "connectionsValue", connectionsValue)
		return admission.Denied(fmt.Sprintf("failed to parse connections annotation: %v", err))
	}

	// Validate permissions for each connection secret
	var permissionErrors []string
	for _, secretRef := range connectionSecrets {
		log.V(1).Info("checking permission for secret", "secret", secretRef.name, "namespace", secretRef.namespace)

		hasPermission, err := v.checkSecretPermission(ctx, req, secretRef)
		if err != nil {
			log.Error(err, "error checking permission for secret", "secret", secretRef.name, "namespace", secretRef.namespace)
			return admission.Errored(http.StatusInternalServerError, fmt.Errorf("error checking permission for secret %s/%s: %w", secretRef.namespace, secretRef.name, err))
		}

		if !hasPermission {
			permissionErrors = append(permissionErrors, fmt.Sprintf("%s/%s", secretRef.namespace, secretRef.name))
		}
	}

	if len(permissionErrors) > 0 {
		return admission.Denied(fmt.Sprintf("user does not have permission to access the following connection secrets: %s", strings.Join(permissionErrors, ", ")))
	}

	return admission.Allowed("Connection permissions validated successfully")
}

// secretReference represents a reference to a secret with namespace and name.
type secretReference struct {
	namespace string
	name      string
}

// parseConnectionsAnnotation parses the connections annotation value into a list of secret references.
// The annotation value should be a comma-separated list of fully qualified secret names (namespace/name).
func parseConnectionsAnnotation(value string) ([]secretReference, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}

	parts := strings.Split(value, ",")
	secrets := make([]secretReference, 0)

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		// Parse namespace/name format
		secretParts := strings.Split(part, "/")
		if len(secretParts) != 2 {
			return nil, fmt.Errorf("invalid secret reference format '%s' - expected 'namespace/name'", part)
		}

		namespace := strings.TrimSpace(secretParts[0])
		name := strings.TrimSpace(secretParts[1])

		if namespace == "" || name == "" {
			return nil, fmt.Errorf("invalid secret reference '%s' - namespace and name cannot be empty", part)
		}

		secrets = append(secrets, secretReference{
			namespace: namespace,
			name:      name,
		})
	}

	return secrets, nil
}

// checkSecretPermission checks if the user has permission to "get" the specified secret using SubjectAccessReview.
func (v *Validator) checkSecretPermission(ctx context.Context, req *admission.Request, secretRef secretReference) (bool, error) {
	log := logf.FromContext(ctx)

	// Create a SubjectAccessReview to check if the user can "get" the secret
	sar := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   req.UserInfo.Username,
			Groups: req.UserInfo.Groups,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: secretRef.namespace,
				Verb:      "get",
				Group:     "",
				Version:   "v1",
				Resource:  "secrets",
				Name:      secretRef.name,
			},
		},
	}

	// Send the SubjectAccessReview to the API server
	if err := v.Client.Create(ctx, sar); err != nil {
		log.Error(err, "failed to create SubjectAccessReview", "secret", secretRef.name, "namespace", secretRef.namespace)
		return false, fmt.Errorf("failed to create SubjectAccessReview: %w", err)
	}

	// Check the result
	if !sar.Status.Allowed {
		log.V(1).Info("user does not have permission to access secret",
			"secret", secretRef.name,
			"namespace", secretRef.namespace,
			"reason", sar.Status.Reason,
			"evaluationError", sar.Status.EvaluationError,
		)

		return false, nil
	}

	log.V(1).Info("user has permission to access secret", "secret", secretRef.name, "namespace", secretRef.namespace)
	return true, nil
}
