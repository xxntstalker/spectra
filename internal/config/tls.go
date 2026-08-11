package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// TLS для сервисов, которые опрашивает портал.
//
// Внутренние сервисы сплошь и рядом стоят за сертификатами, которые выписал
// корпоративный центр сертификации или они сами. Системный набор корней о них
// не знает, и опрос упирается в ошибку проверки — при том что сервис жив и
// документацию отдаёт.
//
// Способа два, и они не равнозначны:
//
//	CABundle       — сказать порталу, кому верить. Проверка остаётся.
//	TLSSkipVerify  — не проверять вовсе. Соединение остаётся шифрованным, но
//	                 подтвердить, что на том конце тот, за кого он себя выдаёт,
//	                 уже нельзя.
//
// Первый способ предпочтителен, и в кластере файл корневого сертификата обычно
// уже где-то лежит. Второй существует потому, что бывает и так, что достать
// его неоткуда, а портал должен работать сегодня.
//
// К Kubernetes это не относится: там свой CA из ServiceAccount, и ослаблять его
// проверку нельзя — вместе с запросом уходит токен, и отправить его в ответ
// первому встречному значит отдать доступ к кластеру.
func (h HTTP) TLSClientConfig() *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if h.TLSSkipVerify {
		cfg.InsecureSkipVerify = true
		return cfg
	}
	if h.caPool != nil {
		cfg.RootCAs = h.caPool
	}
	return cfg
}

// loadCABundle добавляет указанные сертификаты к системным, а не заменяет их:
// корпоративный центр сертификации обычно нужен вдобавок к публичным, а не
// вместо них.
func loadCABundle(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("SPECTRA_CA_BUNDLE: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("SPECTRA_CA_BUNDLE: в %s не нашлось ни одного сертификата "+
			"(ожидается PEM, блоки BEGIN CERTIFICATE)", path)
	}
	return pool, nil
}
