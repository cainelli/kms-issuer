/*
Copyright 2020 Skyscanner Limited.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	kmsiapi "github.com/Skyscanner/kms-issuer/api/v1alpha1"
	"github.com/Skyscanner/kms-issuer/pkg/kmsca"
	"github.com/go-logr/logr"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const (
	// DefaultCertDuration is the default duration the CA certificate is valid for.
	DefaultCertDuration = time.Hour * 24 * 365 * 3 // 3 years
	// DefaultCertRenewalRatio is the default period of time before the CA cetificate is renewed.
	DefaultCertRenewalRatio = 2 / 3
)

// KMSIssuerReconciler reconciles a KMSIssuer object.
type KMSIssuerReconciler struct {
	client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	KMSCA    *kmsca.KMSCA
}

// Annotation for generating RBAC role for writing Events
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// +kubebuilder:rbac:groups=cert-manager.skyscanner.net,resources=kmsissuers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cert-manager.skyscanner.net,resources=kmsissuers/status,verbs=get;update;patch

// NewKMSIssuerReconciler Initialise a new KMSIssuerReconciler
func NewKMSIssuerReconciler(mgr manager.Manager, ca *kmsca.KMSCA) *KMSIssuerReconciler {
	return &KMSIssuerReconciler{
		Client:   mgr.GetClient(),
		Log:      ctrl.Log.WithName("controllers").WithName("kmsissuer_controller"),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("kmsissuer_controller"),
		KMSCA:    ca,
	}
}

// Reconcile KMSIssuer resources.
func (r *KMSIssuerReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("kms-issuer", req.NamespacedName)

	// retrieve the KMSIssuer resource to reconcile.
	issuer := &kmsiapi.KMSIssuer{}
	if err := r.Client.Get(ctx, req.NamespacedName, issuer); err != nil {
		log.Error(err, "failed to retrieve KMSIssuer resource")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// validation
	if issuer.Spec.KeyID == "" {
		return ctrl.Result{}, r.manageFailure(ctx, log, issuer, errors.New("INVALID KeyId"), fmt.Sprintf("Not a valid key: %s", issuer.Spec.KeyID))
	}
	// set default values
	setIssuerDefaultValues(issuer)

	// Renew the certificate
	certInput := desiredCertificateAuthorityCertificateInput(issuer)
	desiredCert := r.KMSCA.GenerateCertificateAuthorityCertificate(certInput)

	if certificateNeedsRenewal(issuer, desiredCert) {
		log.Info("generate certificate")
		cert, err := r.KMSCA.GenerateAndSignCertificateAuthorityCertificate(certInput)
		if err != nil {
			return ctrl.Result{}, r.manageFailure(ctx, log, issuer, err, "Failed to generate the Certificate Authority Certificate")
		}
		issuer.Status.Certificate = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
		if err := r.Client.Status().Update(ctx, issuer); err != nil {
			return ctrl.Result{}, r.manageFailure(ctx, log, issuer, err, "Failed to update the issuer with the issued Certificate")
		}
	}
	return ctrl.Result{
		RequeueAfter: time.Until(desiredCert.NotAfter.Add(-1 * issuer.Spec.RenewBefore.Duration)),
	}, r.manageSuccess(ctx, log, issuer)
}

// setIssuerDefaultValues
func setIssuerDefaultValues(issuer *kmsiapi.KMSIssuer) {
	if issuer.Spec.Duration == nil || issuer.Spec.Duration.Duration == 0 {
		issuer.Spec.Duration = &metav1.Duration{Duration: DefaultCertDuration}
	}
	if issuer.Spec.RenewBefore == nil {
		issuer.Spec.RenewBefore = &metav1.Duration{
			Duration: issuer.Spec.Duration.Duration * DefaultCertRenewalRatio,
		}
	}
	if issuer.Spec.RenewBefore.Duration > issuer.Spec.Duration.Duration {
		issuer.Spec.RenewBefore = &metav1.Duration{
			Duration: issuer.Spec.Duration.Duration * DefaultCertRenewalRatio,
		}
	}
}

// ParseCertificate parse the x509 certificate.
// Returns an error if the certificate pem is invalid.
func ParseCertificate(cert []byte) (*x509.Certificate, error) {
	// parse the certficate
	block, _ := pem.Decode(cert)
	if block == nil {
		return nil, errors.New("failed to parse certificate PEM")
	}
	return x509.ParseCertificate(block.Bytes)
}

// certificateNeedsRenewal returns True if the certificate needs to be created/renewed.
func certificateNeedsRenewal(issuer *kmsiapi.KMSIssuer, desiredCert *x509.Certificate) bool {
	// Check if the certificate hasn't been issued yet.
	if len(issuer.Status.Certificate) == 0 {
		return true
	}
	// Check if the existing cetificate is valid.
	actualCert, err := ParseCertificate(issuer.Status.Certificate)
	if err != nil {
		return true
	}
	// Check if it is time to renew the certificate
	if time.Until(actualCert.NotAfter.Add(-1*issuer.Spec.RenewBefore.Duration)) == 0 {
		return true
	}

	// Check if the certificate has changed
	if desiredCert.SerialNumber != actualCert.SerialNumber {
		return true
	}
	return false
}

// desiredCertificateAuthorityCertificateInput returns the desired cert input
func desiredCertificateAuthorityCertificateInput(issuer *kmsiapi.KMSIssuer) *kmsca.GenerateCertificateAuthorityCertificateInput {
	return &kmsca.GenerateCertificateAuthorityCertificateInput{
		KeyID: issuer.Spec.KeyID,
		Subject: pkix.Name{
			CommonName: issuer.Spec.CommonName,
		},
		Duration: issuer.Spec.Duration.Duration,
		Rounding: issuer.Spec.Duration.Duration - issuer.Spec.RenewBefore.Duration,
	}
}

// SetupWithManager is pre-generated
func (r *KMSIssuerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kmsiapi.KMSIssuer{}).
		Complete(r)
}

// manageSuccess
func (r *KMSIssuerReconciler) manageSuccess(ctx context.Context, log logr.Logger, issuer *kmsiapi.KMSIssuer) error {
	reason := kmsiapi.KMSIssuerReasonIssued
	msg := ""
	log.Info("successfully reconciled issuer")
	r.Recorder.Event(issuer, core.EventTypeNormal, reason, msg)
	ready := kmsiapi.NewCondition(kmsiapi.ConditionReady, kmsiapi.ConditionTrue, reason, msg)
	issuer.Status.SetCondition(&ready)
	return r.Client.Status().Update(ctx, issuer)
}

// manageFailure
func (r *KMSIssuerReconciler) manageFailure(ctx context.Context, log logr.Logger, issuer *kmsiapi.KMSIssuer, issue error, message string) error {
	reason := kmsiapi.KMSIssuerReasonFailed
	log.Error(issue, message)
	r.Recorder.Event(issuer, core.EventTypeWarning, reason, message)
	ready := kmsiapi.NewCondition(kmsiapi.ConditionReady, kmsiapi.ConditionFalse, reason, message)
	issuer.Status.SetCondition(&ready)
	return r.Client.Status().Update(ctx, issuer)
}
