package service

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCPAImportMarksJobFailedWhenAccountsCannotPersist(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/management/auth-files/download" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"imported-token"}`))
	}))
	defer server.Close()

	backend := newFailingStorageBackend(t)
	proxy := NewProxyService(testAccountConfig{})
	accounts := NewAccountService(backend, testAccountConfig{}, proxy, NewLogService(backend))
	config := NewCPAConfig(backend)
	pool, _ := config.AddPool("test", server.URL, "secret")
	config.SetImportJob(pool["id"].(string), newImportJob(1))
	backend.failAccounts = true

	service := NewCPAImportService(config, accounts, proxy)
	service.runImport(pool["id"].(string), pool, []string{"account.json"})
	job := config.GetImportJob(pool["id"].(string))
	if job["status"] != "failed" || len(anyList(job["errors"])) == 0 {
		t.Fatalf("import job = %#v, want failed persistence error", job)
	}
}
