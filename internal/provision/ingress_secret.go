package provision

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func ingressTLSSecretExactMatch(secretType string, tlsCert, tlsKey []byte, pki ingressPKI) error {
	if secretType != "kubernetes.io/tls" {
		return fmt.Errorf("ingress TLS Secret type = %q, want kubernetes.io/tls", secretType)
	}
	if !bytes.Equal(tlsCert, pki.TLSCertPEM) || !bytes.Equal(tlsKey, pki.TLSKeyPEM) {
		return errors.New("ingress TLS Secret does not match the on-disk leaf pair")
	}
	return nil
}

func ingressTLSSecretExactObjectMatch(object map[string]any, pki ingressPKI) error {
	secretType, _, _ := unstructured.NestedString(object, "type")
	tlsCert, _, _ := unstructured.NestedString(object, "data", "tls.crt")
	tlsKey, _, _ := unstructured.NestedString(object, "data", "tls.key")
	decodedCert, err := base64.StdEncoding.DecodeString(tlsCert)
	if err != nil {
		return errors.New("ingress TLS Secret does not match the on-disk leaf pair")
	}
	decodedKey, err := base64.StdEncoding.DecodeString(tlsKey)
	if err != nil {
		return errors.New("ingress TLS Secret does not match the on-disk leaf pair")
	}
	return ingressTLSSecretExactMatch(secretType, decodedCert, decodedKey, pki)
}
