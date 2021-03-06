package pki

import (
	"context"
	"fmt"
	"strings"
	"time"

	kubeservices "github.com/jenkins-x/jx/pkg/kube/services"
	certmng "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1alpha1"
	certclient "github.com/jetstack/cert-manager/pkg/client/clientset/versioned"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

const certSecretPrefix = "tls-"

// WaitCertificateIssuedReady wait for a certificate issued by cert-manager until is ready or the timeout is reached
func WaitCertificateIssuedReady(client certclient.Interface, name string, ns string, timeout time.Duration) error {
	err := wait.PollImmediate(time.Second, timeout, func() (bool, error) {
		cert, err := client.Certmanager().Certificates(ns).Get(name, metav1.GetOptions{})
		if err != nil {
			logrus.Warnf("Failed getting certificate %q: %v", name, err)
			return false, nil
		}
		isReady := cert.HasCondition(certmng.CertificateCondition{
			Type:   certmng.CertificateConditionReady,
			Status: certmng.ConditionTrue,
		})
		if !isReady {
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return errors.Wrapf(err, "waiting for certificate %q to be ready in namespace %q", name, ns)
	}
	return nil
}

// CleanCertSecrets removes all secrets which hold a TLS certificate issued by cert-manager
func CleanCertSecrets(client kubernetes.Interface, ns string) error {
	// delete the tls related secrets so we dont reuse old ones when switching from http to https
	secrets, err := client.CoreV1().Secrets(ns).List(metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, s := range secrets.Items {
		if strings.HasPrefix(s.Name, certSecretPrefix) {
			err := client.CoreV1().Secrets(ns).Delete(s.Name, &metav1.DeleteOptions{})
			if err != nil {
				return fmt.Errorf("failed to delete tls secret %s: %v", s.Name, err)
			}
		}
	}
	return nil
}

// Certificate keeps some information related to a certificate issued by cert-manager
type Certificate struct {
	// Name certificate name
	Name string
	//Namespace certificate namespace
	Namespace string
}

// String returns the certificate information in a string format
func (c Certificate) String() string {
	return fmt.Sprintf("%s/%s", c.Namespace, c.Name)
}

// WatchCertificatesIssuedReady starts watching for ready certificate in the given namespace.
// If the namespace is empty, it will watch the entire cluster. The caller can stop watching by cancelling the context.
func WatchCertificatesIssuedReady(ctx context.Context, client certclient.Interface, ns string) (<-chan Certificate, error) {
	watcher, err := client.Certmanager().Certificates(ns).Watch(metav1.ListOptions{})
	if err != nil {
		return nil, errors.Wrapf(err, "watching certificates in namespace %q", ns)
	}
	results := make(chan Certificate)
	go func() {
		for {
			select {
			case <-ctx.Done():
				watcher.Stop()
				return
			case e := <-watcher.ResultChan():
				if e.Type == watch.Added || e.Type == watch.Modified {
					cert, ok := e.Object.(*certmng.Certificate)
					if ok {
						isReady := cert.HasCondition(certmng.CertificateCondition{
							Type:   certmng.CertificateConditionReady,
							Status: certmng.ConditionTrue,
						})
						if isReady {
							result := Certificate{
								Name:      cert.GetName(),
								Namespace: cert.GetNamespace(),
							}
							results <- result
						}
					}
				}
			}
		}
	}()

	return results, nil
}

// ToCertificates converts a list of services into a list of certificates. The certificate name is built from
// the application label of the service.
func ToCertificates(services []*v1.Service) []Certificate {
	result := make([]Certificate, 0)
	for _, svc := range services {
		app := kubeservices.ServiceAppName(svc)
		cert := certSecretPrefix + app
		ns := svc.GetNamespace()
		result = append(result, Certificate{
			Name:      cert,
			Namespace: ns,
		})
	}
	return result
}
