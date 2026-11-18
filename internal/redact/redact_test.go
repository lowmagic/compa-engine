package redact_test

import (
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/internal/redact"
)

// Secrets is one example of each kind of secret the policy names, with the
// value that must not survive and the placeholder kind that replaces it.
var secrets = []struct{ text, value, kind string }{
	{"-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA7bq9\n-----END RSA PRIVATE KEY-----", "MIIEowIBAAKCAQEA7bq9", "private_key"},
	{`"private_key": "-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkqh\n-----END PRIVATE KEY-----\n"`, "MIIEvQIBADANBgkqh", "private_key"},
	{"id_token: eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJhZG1pbiJ9.c2lnbmF0dXJlLXZhbHVl", "eyJzdWIiOiJhZG1pbiJ9", "token"},
	{"oc login --token=sha256~Xk3vJq9mP2rT8wLz5nB7cD1fG4hY6uE0", "sha256~Xk3vJq9mP2rT8wLz5nB7cD1fG4hY6uE0", "token"},
	{"Authorization: Bearer 9f8e7d6c5b4a39281706f5e4", "9f8e7d6c5b4a39281706f5e4", "token"},
	{"DATABASE_URL=postgres://admin:hunter2pass@db.example.com:5432/app", "hunter2pass", "password"},
	{`{"auths":{"quay.io":{"auth":"dXNlcjpwYXNzd29yZA==","email":"x@example.com"}}}`, "dXNlcjpwYXNzd29yZA==", "pull_secret"},
	{"    client-key-data: LS0tLS1CRUdJTiBSU0EgUFJJVkFURSBLRVktLS0tLQ==", "LS0tLS1CRUdJTiBSU0EgUFJJVkFURSBLRVktLS0tLQ==", "credential"},
	{"aws_access_key_id = AKIAIOSFODNN7EXAMPLE", "AKIAIOSFODNN7EXAMPLE", "cloud_credential"},
	{"aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "secret"},
	{"  password: s3cr3tP@ss", "s3cr3tP@ss", "secret"},
	{"export DB_PASSWORD=Sup3rS3cret", "Sup3rS3cret", "secret"},
	{"  tls.key: c2VjcmV0LWtleS1ieXRlcw==", "c2VjcmV0LWtleS1ieXRlcw==", "secret"},
	// A password's shape does not decide whether it is one.
	{"Temporary admin password: 867530912345", "867530912345", "secret"},
	{"export DB_PASSWORD=12345678", "12345678", "secret"},
	{`password = "correct horse battery"`, "horse battery", "secret"},
	{"client_secret: 'two words'", "two words", "secret"},
	{"password: |\n  SyntheticBlockSecret-ABC\n  second line of it", "SyntheticBlockSecret-ABC", "secret"},
	{"- name: admin\n  password: >-\n    folded secret\n\n    value", "folded secret", "secret"},
	// Encoded and escaped, as a MachineConfig, an installer log and an
	// install state write them.
	{"        source: data:,%7B%22auths%22%3A%7B%22quay.io%22%3A%7B%22auth%22%3A%22dXNlcjpwYXNzd29yZA%22%2C%22email%22%3A%22x%40example.com%22%7D%7D%7D", "dXNlcjpwYXNzd29yZA", "pull_secret"},
	{`"source":"data:text/plain;charset=utf-8;base64,SFRUUFNfUFJPWFk9aHR0cDovL3VzZXI6cHIweHlQYXNzQHByb3h5LmV4YW1wbGUuY29tOjMxMjgKTk9fUFJPWFk9LmNsdXN0ZXIubG9jYWwK"`, "SFRUUFNfUFJPWFk9aHR0cDovL3VzZXI6cHIweHlQYXNzQHByb3h5LmV4YW1wbGUuY29tOjMxMjgKTk9fUFJPWFk9LmNsdXN0ZXIubG9jYWwK", "password"},
	{`level=info msg="Login to the console with user: \"kubeadmin\", and password: \"AbCdE-FgHiJ-KlMnO-PqRsT\""`, "AbCdE-FgHiJ-KlMnO-PqRsT", "secret"},
	// A quoted value runs to its own closing quote: an escaped quote inside
	// it, or YAML's doubled single quote, does not end it.
	{`{"password":"abc\"TAIL-LEAK-123"}`, "TAIL-LEAK-123", "secret"},
	{`password: 'abc''TAIL-LEAK-123'`, "TAIL-LEAK-123", "secret"},
	// A backslash inside an unquoted value is part of it.
	{`DB_PASSWORD=a\WholeSecret123`, `a\WholeSecret123`, "secret"},
	{`password: abcd\TAIL-LEAK-123`, "TAIL-LEAK-123", "secret"},
	// A base64 data URL may be percent-encoded as well.
	{"source: data:;base64,cGFzc3dvcmQ6IFN5bnRoZXRpY1NlY3JldDEyMw%3D%3D", "cGFzc3dvcmQ6IFN5bnRoZXRpY1NlY3JldDEyMw%3D%3D", "secret"},
	// A block scalar's header may carry a comment, and grep writes each
	// line with its file and number before it.
	{"password: | # temporary password\n  BlockSecret123", "BlockSecret123", "secret"},
	{"cfg.yaml:1:password: |\ncfg.yaml-2-  GrepBlockSecret123\ncfg.yaml-3-next: 1", "GrepBlockSecret123", "secret"},
	// A quoted value is a string, never a boolean or null; a value under a
	// key ending in its noun is the secret whatever its length.
	{`password: "false"`, `"false"`, "secret"},
	{`password: "none"`, `"none"`, "secret"},
	{"password: abc", "abc", "secret"},
	// bootstrap.ign's control-plane keys, in base64 and URL-encoded data
	// URLs; install-config.yaml's pull secret, whose key takes the whole
	// value; metadata.json's vSphere password.
	{`"source":"data:text/plain;charset=utf-8;base64,LS0tLS1CRUdJTiBSU0EgUFJJVkFURSBLRVktLS0tLQpNSUlFcFN5bnRoZXRpY0Jvb3RzdHJhcEtleUFBQUEKLS0tLS1FTkQgUlNBIFBSSVZBVEUgS0VZLS0tLS0K"`, "LS0tLS1CRUdJTiBSU0EgUFJJVkFURSBLRVktLS0tLQpNSUlFcFN5bnRoZXRpY0Jvb3RzdHJhcEtleUFBQUEKLS0tLS1FTkQgUlNBIFBSSVZBVEUgS0VZLS0tLS0K", "private_key"},
	{`"source":"data:,-----BEGIN%20RSA%20PRIVATE%20KEY-----%0AMIIEpSyntheticBootstrapKeyAAAA%0A-----END%20RSA%20PRIVATE%20KEY-----%0A"`, "MIIEpSyntheticBootstrapKeyAAAA", "private_key"},
	{`pullSecret: '{"auths":{"cloud.openshift.com":{"auth":"b3BlbnNoaWZ0LXN5bnRoZXRpYw==","email":"x@example.com"}}}'`, "b3BlbnNoaWZ0LXN5bnRoZXRpYw==", "secret"},
	{`{"clusterName":"c1","infraID":"c1-x7k2p","vsphere":{"username":"administrator@vsphere.local","password":"Synth3tic!Pass"}}`, "Synth3tic!Pass", "secret"},
	{`"pullSecret": "{\"auths\":{\"quay.io\":{\"auth\":\"dXNlcjpwYXNzd29yZA==\"}}}"`, "dXNlcjpwYXNzd29yZA==", "secret"},
}

func TestEachSecretIsRedacted(t *testing.T) {
	p := redact.For("external_provider")
	for _, s := range secrets {
		got := p.String(s.text)
		if strings.Contains(got, s.value) || !strings.Contains(got, "[REDACTED:"+s.kind+"]") {
			t.Errorf("%q\n  became %q, want the %s replaced", s.text, got, s.kind)
		}
		if again := p.String(got); again != got {
			t.Errorf("redacting twice changed %q to %q", got, again)
		}
		// Holds sees what redaction replaces, and nothing once it has.
		if _, ok := p.Holds(s.text); !ok {
			t.Errorf("Holds missed %q", s.text)
		}
		if kind, ok := p.Holds(got); ok {
			t.Errorf("Holds found a %s in the redacted %q", kind, got)
		}
	}
}

// A block scalar's value is replaced and the keys after it are kept;
// under secrets-5 its other lines stay, empty, so the text keeps its line
// count, and under secrets-4 they went.
func TestBlockValueEnds(t *testing.T) {
	text := "auth:\n  password: |\n    line one\n    line two\n  user: bob\nnext: 3\n"
	want := "auth:\n  password: |\n    [REDACTED:secret]\n\n  user: bob\nnext: 3\n"
	if got := redact.For("external_provider").String(text); got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	old, _ := redact.ByVersion("secrets-4")
	if got := old.String(text); got != "auth:\n  password: |\n    [REDACTED:secret]\n  user: bob\nnext: 3\n" {
		t.Errorf("secrets-4: got %q", got)
	}
	grep := "cfg.yaml:1:password: |\ncfg.yaml-2-  one\ncfg.yaml-3-  two\ncfg.yaml-4-user: bob\nother.yaml:9:x: 1"
	wantGrep := "cfg.yaml:1:password: |\ncfg.yaml-2-  [REDACTED:secret]\ncfg.yaml-3-\ncfg.yaml-4-user: bob\nother.yaml:9:x: 1"
	if got := redact.For("external_provider").String(grep); got != wantGrep {
		t.Errorf("grep's output: got %q\nwant %q", got, wantGrep)
	}
}

// What names a secret, or cannot be one, is kept, and so are hostnames,
// addresses and names: the policy is secrets only.
func TestNonSecretsAreKept(t *testing.T) {
	p := redact.For("external_provider")
	for _, text := range []string{
		"server: https://api.cluster-7.example.com:6443",
		"nodeIP: 10.0.0.12",
		"secretName: router-certs-default",
		"tokenSecretRef: {name: bound-sa-token}",
		"max_tokens: 4096",
		"token_limit: 100",
		"secretCount: 3",
		"tokenTTL: 3600",
		"secretName: |\n  router-certs-default",
		"automountServiceAccountToken: false",
		"customer: Acme Corp, cluster prod-east",
		"https://quay.io/openshift-release-dev/ocp-release@sha256:" + strings.Repeat("ab", 32),
		"[REDACTED:secret] and https://user:[REDACTED:password]@host",
		`token_endpoint: "https://sso.example.test/token"`,
		`{"token_endpoint":"https://sso.example.test/token","introspection_endpoint":"https://sso.example.test/introspect"}`,
		"secretProviderURL: https://vault.example.test",
		"tokenIssuer: https://issuer.example.test",
		"password: false",
		"password: null",
		"passwordAuthentication: no",
		"password: |\n  [REDACTED:secret]",
		"        source: data:,%5BUnit%5D%0ADescription%3DKubelet%0A",
		"        source: data:text/plain;charset=utf-8;base64,W1VuaXRdCkRlc2NyaXB0aW9uPUt1YmVsZXQK",
		`msg="Login to the console with user: \"kubeadmin\""`,
	} {
		if got := p.String(text); got != text {
			t.Errorf("%q became %q", text, got)
		}
	}
}

func TestPolicies(t *testing.T) {
	if p := redact.For("local_only"); p.Version != redact.VersionNone || p.String(secrets[0].text) != secrets[0].text {
		t.Errorf("local_only redacts nothing: %+v", p)
	}
	for _, egress := range []string{"internal_network", "external_provider", ""} {
		if redact.For(egress).Version != redact.VersionSecrets {
			t.Errorf("%q must get the secrets policy", egress)
		}
	}
	for _, v := range []string{redact.VersionNone, redact.VersionSecrets} {
		if p, ok := redact.ByVersion(v); !ok || p.Version != v {
			t.Errorf("version %s", v)
		}
	}
	if _, ok := redact.ByVersion("v0"); ok {
		t.Error("an unknown version resolved")
	}
}

func TestJSON(t *testing.T) {
	p := redact.For("external_provider")
	out, err := p.JSON([]byte(`{"password":"x","text":"` + strings.ReplaceAll(secrets[4].text, `"`, `\"`) + `"}`))
	if err != nil || strings.Contains(string(out), secrets[4].value) || !strings.Contains(string(out), `"password":"x"`) {
		t.Errorf("JSON: %s %v", out, err)
	}
	if out, _ := p.JSON([]byte("not json, " + secrets[10].text)); strings.Contains(string(out), secrets[10].value) {
		t.Errorf("text payload: %s", out)
	}
}

// Under secrets-5 every replacement keeps the line breaks it covers, so a
// line number written after redaction is the stored line's.
func TestRedactionKeepsLines(t *testing.T) {
	key := "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\nabcdef\n-----END RSA PRIVATE KEY-----"
	for _, text := range []string{
		"a\n" + key + "\nb: 1\n",
		"cfg:\n  password: |\n    one\n\n    two\n  user: x\n",
		"token: eyJhbGciOi.eyJzdWIiOi.SflKxwRJSM\nnext\n",
	} {
		got := redact.For("external_provider").String(text)
		if got == text || strings.Count(got, "\n") != strings.Count(text, "\n") {
			t.Errorf("%q: %d lines became %d: %q", text, strings.Count(text, "\n"), strings.Count(got, "\n"), got)
		}
	}
	if redact.VersionSecrets != "secrets-5" {
		t.Errorf("version %s", redact.VersionSecrets)
	}
}
