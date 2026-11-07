package tokenledger

import (
	"encoding/json"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

const FixtureSpendContract = "musicops.fixture.v1"

// SpendProof is a correlated charge record. Live DicMusic and Orpheus do not
// have a verified per-request history schema in this repository, so only the
// existing fixture contract can move a reservation. ProductionVerified stays false.
type SpendProof struct {
	Contract           string `json:"contract"`
	AccountID          string `json:"account_id"`
	ResourceID         string `json:"resource_id"`
	RequestID          string `json:"request_id"`
	State              string `json:"state"`
	ProductionVerified bool   `json:"production_verified"`
}

// ParseSpend accepts only musicops.fixture.v1 with account, resource and request.
// Missing fields, another contract, or an incomplete page stay uncertain and change nothing.
func ParseSpend(body []byte) (SpendProof, string) {
	if len(body) == 0 {
		return SpendProof{}, ClassUncertain
	}
	var proof SpendProof
	if json.Unmarshal(body, &proof) != nil {
		return SpendProof{}, "parse"
	}
	if proof.Contract != FixtureSpendContract || proof.AccountID == "" || proof.ResourceID == "" || proof.RequestID == "" {
		return SpendProof{}, ClassUncertain
	}
	switch proof.State {
	case "CONSUMED", "NOT_CONSUMED", "UNKNOWN":
		proof.ProductionVerified = false
		return proof, ""
	default:
		return SpendProof{}, ClassUncertain
	}
}

// Reconcile applies an explicit correlated record. A torrent body, HTTP 200, or
// a balance delta is not a record and must not be passed here.
func (l *Ledger) Reconcile(now time.Time, site string, proof SpendProof) (Result, error) {
	if proof.Contract != FixtureSpendContract || proof.State == "UNKNOWN" || proof.State == "" {
		return Result{Class: ClassUncertain, Scope: "request", Basis: "扣费记录不能关联账号、资源和请求，保持不确定"}, nil
	}
	var current row
	var found bool
	err := l.engine.View(func(v *dataengine.View) error {
		id, ok, err := readRequest(v, site, proof.RequestID)
		if err != nil || !ok {
			return err
		}
		raw, ok, err := v.Get(rowKey(id))
		if err != nil || !ok {
			return err
		}
		found = true
		if json.Unmarshal(raw, &current) != nil {
			return dataengine.ErrInvalidRequest
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	if !found || current.AccountID != proof.AccountID || current.ResourceID != proof.ResourceID || current.RequestID != proof.RequestID {
		return Result{Class: ClassUncertain, Scope: "request", Basis: "扣费记录与预约的账号或资源不一致，不改账"}, nil
	}
	switch current.State {
	case StateConfirmed:
		if proof.State == "CONSUMED" {
			return resultFrom(current), nil
		}
		return Result{Class: ClassUncertain, State: current.State, Scope: "request", Basis: "已确认的消耗不接受相反的扣费记录"}, nil
	case StateReleased, StateRejected:
		if current.State == StateReleased && proof.State == "NOT_CONSUMED" {
			return resultFrom(current), nil
		}
		return Result{Class: ClassUncertain, State: current.State, Scope: "request", Basis: "已结束的预约不接受相反的扣费记录"}, nil
	}
	switch proof.State {
	case "CONSUMED":
		return l.Advance(now, site, proof.RequestID, StateConfirmed, "fixture-correlated")
	case "NOT_CONSUMED":
		return l.Advance(now, site, proof.RequestID, StateReleased, "fixture-not-consumed")
	default:
		return Result{Class: ClassUncertain, Scope: "request", Basis: "扣费记录状态不确定，不重发也不释放"}, nil
	}
}
