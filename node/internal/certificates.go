package node

import "crypto/x509"

func platformCertificatePool() *x509.CertPool {
	pool, _ := x509.SystemCertPool()
	if pool == nil {
		pool = x509.NewCertPool()
	}
	return pool
}
