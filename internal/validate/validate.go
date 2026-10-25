// Package validate holds input validation shared by the API and managers.
package validate

import (
	"regexp"
	"strings"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/errs"
)

var (
	// LXD instance names: letters, digits and dashes, starting with a
	// letter, max 63 chars, no trailing dash. Type3 also requires lowercase
	// so names are safe as hostnames and DNS labels.
	nameRe        = regexp.MustCompile(`^[a-z][a-z0-9-]{0,61}[a-z0-9]$`)
	ownerRe       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,127}$`)
	labelKeyRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,62}$`)
	snapshotRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	fingerprintRe = regexp.MustCompile(`^[a-f0-9]{12,64}$`)
	aliasRe       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:-]{0,127}$`)
	sshKeyRe      = regexp.MustCompile(`^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)|sk-ssh-ed25519@openssh\.com) [A-Za-z0-9+/=]{16,}( [^\n\r]{0,256})?$`)
	idempotencyRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)
)

// Name validates a host or container name.
func Name(field, v string) error {
	if !nameRe.MatchString(v) || strings.Contains(v, "--") {
		return errs.Invalid("invalid_"+field, "%s must be 2-63 lowercase letters, digits or single dashes, starting with a letter", field).
			WithDetails(map[string]any{"field": field})
	}
	return nil
}

// OwnerID validates a customer identifier.
func OwnerID(v string) error {
	if !ownerRe.MatchString(v) {
		return errs.Invalid("invalid_owner_id", "owner_id must be 1-128 characters of letters, digits, '.', '_', '@' or '-'").
			WithDetails(map[string]any{"field": "owner_id"})
	}
	return nil
}

// Labels validates host label keys and values.
func Labels(labels map[string]string) error {
	if len(labels) > 32 {
		return errs.Invalid("invalid_labels", "at most 32 labels are allowed")
	}
	for k, v := range labels {
		if !labelKeyRe.MatchString(k) {
			return errs.Invalid("invalid_labels", "label key %q is invalid", k)
		}
		if len(v) > 256 || strings.ContainsAny(v, "\n\r\x00") {
			return errs.Invalid("invalid_labels", "label %q value is invalid", k)
		}
	}
	return nil
}

// SnapshotName validates a snapshot name.
func SnapshotName(v string) error {
	if !snapshotRe.MatchString(v) {
		return errs.Invalid("invalid_snapshot_name", "snapshot name must be 1-63 letters, digits, '.', '_' or '-'")
	}
	return nil
}

// Fingerprint validates a (possibly abbreviated, >= 12 hex chars) image fingerprint.
func Fingerprint(v string) error {
	if !fingerprintRe.MatchString(v) {
		return errs.Invalid("invalid_fingerprint", "fingerprint must be 12-64 lowercase hex characters")
	}
	return nil
}

// Alias validates an image alias.
func Alias(v string) error {
	if !aliasRe.MatchString(v) || strings.Contains(v, "..") {
		return errs.Invalid("invalid_alias", "image alias %q is invalid", v)
	}
	return nil
}

// SSHKeys validates authorized_keys lines.
func SSHKeys(keys []string) error {
	if len(keys) > 16 {
		return errs.Invalid("invalid_ssh_keys", "at most 16 SSH keys are allowed")
	}
	for i, k := range keys {
		if len(k) > 8192 || !sshKeyRe.MatchString(strings.TrimSpace(k)) {
			return errs.Invalid("invalid_ssh_keys", "ssh_authorized_keys[%d] is not a valid public key", i)
		}
	}
	return nil
}

// IdempotencyKey validates an Idempotency-Key header value.
func IdempotencyKey(v string) error {
	if v != "" && !idempotencyRe.MatchString(v) {
		return errs.Invalid("invalid_idempotency_key", "Idempotency-Key must be 8-128 characters of letters, digits, '.', '_', ':' or '-'")
	}
	return nil
}

// Reason validates a free-text justification for a destructive action.
func Reason(v string) error {
	if strings.TrimSpace(v) == "" {
		return errs.Invalid("reason_required", "a reason is required for this action")
	}
	if len(v) > 512 || strings.ContainsAny(v, "\x00") {
		return errs.Invalid("invalid_reason", "reason must be at most 512 characters")
	}
	return nil
}

// Range checks lo <= v <= hi.
func Range(field string, v, lo, hi int) error {
	if v < lo || v > hi {
		return errs.Invalid("out_of_range", "%s must be between %d and %d", field, lo, hi).
			WithDetails(map[string]any{"field": field, "min": lo, "max": hi, "value": v})
	}
	return nil
}
