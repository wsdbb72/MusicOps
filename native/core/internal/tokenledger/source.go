package tokenledger

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
)

// SpendSource supplies correlated charge records for a token request. It is
// the seam a real per-request ledger plugs into: the acquire path asks the
// source for a proof after the fetch, and only a proof that Reconcile accepts
// (same account, resource and request, fixture contract) settles the
// reservation. A source that has no record for a request returns found=false
// and changes nothing; the reservation stays REQUEST_UNKNOWN.
//
// No live source is registered: the only source shipped here reads fixture
// proofs from a file the operator writes. A real online ledger format has
// not been verified (see the migration plan), so nothing here pretends to
// read one.
type SpendSource interface {
	Spend(ctx context.Context, site, requestID string) (SpendProof, bool, error)
}

// FileSpendSource reads fixture-contract proofs from a JSON-lines file, one
// proof per line. The file is read fresh on every call, so proofs written
// after the process started are picked up without a restart. A missing file
// means no source is configured and always returns found=false. Lines that
// do not parse as fixture proofs are skipped: one bad line must not block
// every other proof. The site is not part of a proof; Reconcile checks the
// account, resource and request against the reservation.
type FileSpendSource struct {
	Path string
}

// NewFileSpendSource returns a source reading proofs from path.
func NewFileSpendSource(path string) *FileSpendSource {
	return &FileSpendSource{Path: path}
}

// Spend returns the first proof in the file whose request id matches.
func (s *FileSpendSource) Spend(ctx context.Context, site, requestID string) (SpendProof, bool, error) {
	if s == nil || s.Path == "" || requestID == "" {
		return SpendProof{}, false, nil
	}
	file, err := os.Open(s.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SpendProof{}, false, nil
		}
		return SpendProof{}, false, err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	for {
		select {
		case <-ctx.Done():
			return SpendProof{}, false, ctx.Err()
		default:
		}
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if proof, class := ParseSpend(line); class == "" && proof.RequestID == requestID {
				return proof, true, nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return SpendProof{}, false, nil
			}
			return SpendProof{}, false, err
		}
	}
}
