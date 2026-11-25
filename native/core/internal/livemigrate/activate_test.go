package livemigrate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
	"github.com/wsdbb72/MusicOps/native/core/internal/tokenledger"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

func TestActivateUsesSourceFingerprintBeforeHashingOrOpeningSQLite(t *testing.T) {
	profile := t.TempDir()
	state := filepath.Join(profile, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	sqlite := filepath.Join(state, "musicops.sqlite")
	if err := os.WriteFile(sqlite, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(sqlite)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := dataengine.Open(filepath.Join(state, "native"))
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	receipt, err := json.Marshal(storedReceipt{Report: Report{
		SourceSHA256:    "cached-source",
		SourceSize:      info.Size(),
		SourceModTimeNS: info.ModTime().UnixNano(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := engine.CompareAndSwap(
		[]dataengine.Condition{{Key: []byte(receiptKey), Absent: true}},
		[]dataengine.Mutation{{Key: []byte(receiptKey), Value: receipt}},
	); err != nil || !ok {
		t.Fatalf("seed receipt: ok=%v err=%v", ok, err)
	}
	tasks := taskrepo.New(engine)
	result, err := Activate(engine, profile, workflow.New(engine, tasks), tokenledger.New(engine), time.Now())
	if err != nil {
		t.Fatalf("fingerprint fast path tried to open invalid SQLite: %v", err)
	}
	if !result.Replayed || result.SourceSHA256 != "cached-source" || result.SourceSize != info.Size() || result.SourceModTimeNS != info.ModTime().UnixNano() {
		t.Fatalf("result = %+v", result)
	}
}
