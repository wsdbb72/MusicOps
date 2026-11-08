// Transcode is the library audio conversion lane. It converts one PCM WAV
// already inside this process's state directory to FLAC with the system
// ffmpeg, and proves the conversion by decoding both files to raw PCM and
// comparing hashes. The source is never modified or overwritten; the output
// is written to a temp file and renamed into place, and only then recorded.
// DSD and every non-PCM source are refused: only PCM-to-FLAC is implemented,
// matching the Python prepare_media_copy contract.
package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/scheduler"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
)

// transcodeCommand is the durable queue name for library conversion tasks.
// These tasks are not workflow-owned: they have no workflow job, and they run
// on the transcode lane (capacity 1).
const transcodeCommand = "transcode_audio"

const transcodeTimeout = 5 * time.Minute

// transcodePayload is one conversion request. Source is a path inside the
// application state directory; Format is "flac" and nothing else.
type transcodePayload struct {
	RequestID string `json:"request_id"`
	Source    string `json:"source"`
	Format    string `json:"format"`
}

// transcodeReceipt is the durable outcome. Replayed is true when the same
// request id already produced this exact output.
type transcodeReceipt struct {
	RequestID    string `json:"request_id"`
	Source       string `json:"source"`
	SourceSHA256 string `json:"source_sha256"`
	Output       string `json:"output"`
	SHA256       string `json:"sha256"`
	PCMSHA256    string `json:"pcm_sha256"`
	Format       string `json:"format"`
	Replayed     bool   `json:"replayed,omitempty"`
}

func transcodeReceiptKey(id string) []byte { return []byte("transcode/receipt/" + id) }

// transcode converts one PCM WAV to FLAC. A failure fails the task without
// writing a receipt, so a failed conversion stays visible in the queue.
func (a *App) transcode(ctx context.Context, execution *scheduler.Execution, payload []byte) (scheduler.Outcome, error) {
	if err := execution.Checkpoint(ctx, "transcode", 0, "转码"); err != nil {
		return scheduler.Outcome{}, err
	}
	var in transcodePayload
	if err := json.Unmarshal(payload, &in); err != nil || !validTranscodeID(in.RequestID) || in.Format != "flac" || strings.TrimSpace(in.Source) == "" {
		return scheduler.Outcome{State: "failed", ErrorClass: "INVALID_REQUEST", Message: "转码请求无效：只要 PCM WAV 转 FLAC"}, nil
	}
	source, err := resolveInDir(a.Dir, in.Source)
	if err != nil {
		return scheduler.Outcome{State: "failed", ErrorClass: "INVALID_REQUEST", Message: "转码源文件必须在应用数据目录内"}, nil
	}
	decoder := audiorelease.SystemDecoder()
	if decoder == nil {
		return scheduler.Outcome{State: "failed", ErrorClass: "DECODER_MISSING", Message: "转码需要系统 ffmpeg/ffprobe，当前不可用"}, nil
	}
	if err := execution.Checkpoint(ctx, "transcode_validate", 10, "校验源文件"); err != nil {
		return scheduler.Outcome{}, err
	}
	sourceHash, err := hashFile(source)
	if err != nil {
		return scheduler.Outcome{State: "failed", ErrorClass: "INVALID_REQUEST", Message: "转码源文件不可读"}, nil
	}
	if existing, found, err := a.readTranscodeReceipt(in.RequestID); err != nil {
		return scheduler.Outcome{}, err
	} else if found {
		if existing.Format == "flac" && existing.Source == source && existing.SourceSHA256 == sourceHash {
			if err := sameFile(existing.Output, existing.SHA256); err == nil {
				existing.Replayed = true
				encoded, err := json.Marshal(existing)
				if err != nil {
					return scheduler.Outcome{}, err
				}
				return scheduler.Outcome{State: "completed", Payload: encoded, Message: "转码已完成（重放）"}, nil
			}
		}
		return scheduler.Outcome{State: "failed", ErrorClass: "CONFLICT", Message: "同一转码请求对应另一源文件或输出"}, nil
	}
	if err := checkWAVPCM(source); err != nil {
		return scheduler.Outcome{State: "failed", ErrorClass: "INVALID_REQUEST", Message: "只支持 PCM WAV 转 FLAC： " + err.Error()}, nil
	}
	if err := decoder(ctx, source); err != nil {
		return scheduler.Outcome{State: "failed", ErrorClass: "AUDIO_REJECTED", Message: "源文件未通过全解码校验"}, nil
	}
	output := strings.TrimSuffix(source, filepath.Ext(source)) + ".flac"
	if output == source {
		return scheduler.Outcome{State: "failed", ErrorClass: "INVALID_REQUEST", Message: "转码输出与源文件同名"}, nil
	}
	if _, err := os.Stat(output); err == nil {
		return scheduler.Outcome{State: "failed", ErrorClass: "CONFLICT", Message: "转码输出已存在，不能覆盖"}, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return scheduler.Outcome{}, err
	}
	if err := execution.Checkpoint(ctx, "transcode_convert", 40, "正在转码"); err != nil {
		return scheduler.Outcome{}, err
	}
	temp := filepath.Join(filepath.Dir(output), ".musicops-"+in.RequestID+".transcode.flac")
	runCtx, cancel := context.WithTimeout(ctx, transcodeTimeout)
	defer cancel()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return scheduler.Outcome{State: "failed", ErrorClass: "DECODER_MISSING", Message: "转码需要系统 ffmpeg，当前不可用"}, nil
	}
	convert := exec.CommandContext(runCtx, ffmpeg, "-nostdin", "-v", "error", "-xerror", "-i", source, "-map", "0:a:0", "-map_metadata", "0", "-c:a", "flac", "-f", "flac", "-y", temp)
	stderr, err := convert.CombinedOutput()
	if err != nil || strings.TrimSpace(string(stderr)) != "" {
		os.Remove(temp)
		if runCtx.Err() != nil {
			return scheduler.Outcome{}, runCtx.Err()
		}
		return scheduler.Outcome{State: "failed", ErrorClass: "TRANSCODE_FAILED", Message: "转码失败"}, nil
	}
	if err := decoder(ctx, temp); err != nil {
		os.Remove(temp)
		return scheduler.Outcome{State: "failed", ErrorClass: "AUDIO_REJECTED", Message: "转码输出未通过全解码校验"}, nil
	}
	sourcePCM, err := pcmHash(ctx, source)
	if err != nil {
		os.Remove(temp)
		return scheduler.Outcome{}, err
	}
	outputPCM, err := pcmHash(ctx, temp)
	if err != nil {
		os.Remove(temp)
		return scheduler.Outcome{}, err
	}
	if sourcePCM != outputPCM {
		os.Remove(temp)
		return scheduler.Outcome{State: "failed", ErrorClass: "TRANSCODE_FAILED", Message: "转码前后 PCM 内容不一致"}, nil
	}
	if err := os.Rename(temp, output); err != nil {
		os.Remove(temp)
		return scheduler.Outcome{}, err
	}
	digest, err := hashFile(output)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	receipt := transcodeReceipt{RequestID: in.RequestID, Source: source, SourceSHA256: sourceHash, Output: output, SHA256: digest, PCMSHA256: sourcePCM, Format: "flac"}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	ok, err := a.engine.CompareAndSwap(
		[]dataengine.Condition{{Key: transcodeReceiptKey(in.RequestID), Absent: true}},
		[]dataengine.Mutation{{Key: transcodeReceiptKey(in.RequestID), Value: encoded}},
	)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	if !ok {
		// Another run recorded this request first; re-read and verify it
		// matches instead of claiming a second conversion.
		if existing, found, err := a.readTranscodeReceipt(in.RequestID); err != nil {
			return scheduler.Outcome{}, err
		} else if !found || existing.SHA256 != digest || existing.Output != output {
			return scheduler.Outcome{State: "failed", ErrorClass: "CONFLICT", Message: "转码记录已被另一请求占用"}, nil
		} else {
			receipt = existing
			receipt.Replayed = true
			encoded, err = json.Marshal(receipt)
			if err != nil {
				return scheduler.Outcome{}, err
			}
		}
	}
	return scheduler.Outcome{State: "completed", Payload: encoded, Message: "转码完成"}, nil
}

func (a *App) readTranscodeReceipt(id string) (transcodeReceipt, bool, error) {
	var receipt transcodeReceipt
	var found bool
	err := a.engine.View(func(v *dataengine.View) error {
		raw, ok, err := v.Get(transcodeReceiptKey(id))
		if err != nil || !ok {
			return err
		}
		found = true
		return json.Unmarshal(raw, &receipt)
	})
	return receipt, found, err
}

// listTranscodeReceipts returns every recorded conversion.
func (a *App) listTranscodeReceipts() ([]transcodeReceipt, error) {
	var out []transcodeReceipt
	err := a.engine.View(func(v *dataengine.View) error {
		var after []byte
		for {
			page, err := v.Scan([]byte("transcode/receipt/"), after, 200)
			if err != nil {
				return err
			}
			for _, item := range page.Items {
				var receipt transcodeReceipt
				if err := json.Unmarshal(item.Value, &receipt); err != nil {
					return err
				}
				out = append(out, receipt)
			}
			if len(page.Next) == 0 {
				return nil
			}
			after = page.Next
		}
	})
	return out, err
}

func validTranscodeID(id string) bool {
	if len(id) < 1 || len(id) > 80 {
		return false
	}
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

// resolveInDir resolves raw against dir and rejects anything that escapes
// dir, including through symlinks. raw may be absolute or relative to dir.
func resolveInDir(dir, raw string) (string, error) {
	if strings.ContainsRune(raw, 0) {
		return "", dataengine.ErrInvalidRequest
	}
	abs := raw
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(dir, abs)
	}
	abs = filepath.Clean(abs)
	root, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", dataengine.ErrInvalidRequest
	}
	// A symlink inside the directory must not point outside it.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	rel, err = filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", dataengine.ErrInvalidRequest
	}
	return resolved, nil
}

// checkWAVPCM rejects anything that is not a PCM WAV: the Python
// prepare_media_copy contract only implements PCM-to-FLAC, and DSD sources
// need their own policy.
func checkWAVPCM(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	header := make([]byte, 44)
	if _, err := io.ReadFull(file, header); err != nil {
		return errors.New("文件太短，不是 WAV")
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
		return errors.New("不是 WAV 文件")
	}
	if binary.LittleEndian.Uint16(header[20:22]) != 1 {
		return errors.New("不是 PCM 编码")
	}
	if !bytes.Contains(header[12:], []byte("data")) && !bytes.Contains(header, []byte("data")) {
		return errors.New("WAV 数据块缺失")
	}
	return nil
}

// pcmHash decodes one audio file to raw s16le PCM at a fixed rate and channel
// layout and hashes the bytes, so a WAV and its FLAC conversion compare
// equal only when the decoded audio is identical.
func pcmHash(ctx context.Context, path string) (string, error) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", err
	}
	runCtx, cancel := context.WithTimeout(ctx, transcodeTimeout)
	defer cancel()
	command := exec.CommandContext(runCtx, ffmpeg, "-nostdin", "-v", "error", "-xerror", "-i", path, "-map", "0:a:0", "-f", "s16le", "-ac", "2", "-ar", "44100", "-")
	stdout, err := command.StdoutPipe()
	if err != nil {
		return "", err
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return "", err
	}
	sum := sha256.New()
	if _, err := io.Copy(sum, stdout); err != nil {
		_ = command.Wait()
		return "", err
	}
	if err := command.Wait(); err != nil || strings.TrimSpace(stderr.String()) != "" {
		return "", fmt.Errorf("PCM 解码失败")
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func sameFile(path, digest string) error {
	got, err := hashFile(path)
	if err != nil {
		return err
	}
	if got != digest {
		return errors.New("文件内容与记录不一致")
	}
	return nil
}

// transcodeEnqueue validates a conversion request and queues it on the
// transcode lane. The handler does the work; this only creates the task.
func (a *App) transcodeEnqueue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RequestID string `json:"request_id"`
		Source    string `json:"source"`
		Format    string `json:"format"`
	}
	if err := decode(r, &body); err != nil || !validTranscodeID(body.RequestID) || body.Format != "flac" || strings.TrimSpace(body.Source) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	source, err := resolveInDir(a.Dir, body.Source)
	if err != nil || !fileExists(source) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	encoded, err := json.Marshal(transcodePayload{RequestID: body.RequestID, Source: source, Format: "flac"})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "TRANSCODE_UNAVAILABLE"})
		return
	}
	taskID := "transcode:" + body.RequestID
	if _, err := a.tasks.Enqueue("enqueue-"+body.RequestID, taskrepo.Spec{ID: taskID, Command: transcodeCommand, Lane: "transcode", Priority: 1, CreatedNS: a.now().UnixNano()}, encoded); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "TRANSCODE_CONFLICT"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "task_id": taskID, "request_id": body.RequestID})
}

// transcodeList returns every recorded conversion receipt.
func (a *App) transcodeList(w http.ResponseWriter, r *http.Request) {
	receipts, err := a.listTranscodeReceipts()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "TRANSCODE_UNAVAILABLE"})
		return
	}
	if receipts == nil {
		receipts = []transcodeReceipt{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "receipts": receipts, "count": len(receipts)})
}

func fileExists(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}
