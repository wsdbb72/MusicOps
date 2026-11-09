package qbittorrent

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/bits"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ErrInvalidMetainfo is returned when torrent bytes are not a canonical,
// supported metainfo file.
var ErrInvalidMetainfo = errors.New("qbittorrent: invalid metainfo")

const (
	maxMetainfoBytes = 8 << 20
	maxMetainfoFiles = 20000
	maxDecoderDepth  = 64
	maxDecoderNodes  = 200000
)

// MetainfoFile is one file in a parsed metainfo.
type MetainfoFile struct {
	// Path uses "/" separators and includes the torrent root name.
	Path string `json:"path"`
	// Length is the file length in bytes.
	Length int64 `json:"length"`
	// Padding is true for BEP 52 padding files (attr "p").
	Padding bool `json:"padding,omitempty"`
	// PiecesRoot is the hex v2 pieces root, empty for v1-only files.
	PiecesRoot string `json:"pieces_root,omitempty"`
}

// Metainfo is a parsed .torrent file.
type Metainfo struct {
	Name           string         `json:"name"`
	Type           string         `json:"type"` // "v1", "v2" or "hybrid"
	InfoHashV1     string         `json:"infohash_v1,omitempty"`
	InfoHashV2     string         `json:"infohash_v2,omitempty"`
	Files          []MetainfoFile `json:"file_list"`
	PieceLength    int64          `json:"piece_length"`
	TotalSize      int64          `json:"total_size"`
	MetainfoSHA256 string         `json:"metainfo_sha256"`
}

// bDict is an order-preserving bencode dictionary. Key order matters: the
// decoder rejects unordered or duplicate keys, and v2 file-tree walks rely
// on the encoded order.
type bDict struct {
	keys   []string
	values map[string]any
}

func (d *bDict) get(key string) (any, bool) {
	value, ok := d.values[key]
	return value, ok
}

func (d *bDict) has(key string) bool {
	_, ok := d.values[key]
	return ok
}

func dictInt(d *bDict, key string) (int64, bool) {
	value, ok := d.get(key)
	if !ok {
		return 0, false
	}
	n, ok := value.(int64)
	return n, ok
}

func dictBytes(d *bDict, key string) ([]byte, bool) {
	value, ok := d.get(key)
	if !ok {
		return nil, false
	}
	b, ok := value.([]byte)
	return b, ok
}

func dictList(d *bDict, key string) ([]any, bool) {
	value, ok := d.get(key)
	if !ok {
		return nil, false
	}
	list, ok := value.([]any)
	return list, ok
}

func dictDict(d *bDict, key string) (*bDict, bool) {
	value, ok := d.get(key)
	if !ok {
		return nil, false
	}
	dict, ok := value.(*bDict)
	return dict, ok
}

// decoder is a strict bencode decoder. It rejects unordered or duplicate
// dictionary keys and non-canonical integers, mirroring torrentfetch and
// musicops.clients.metainfo.
type decoder struct {
	data      []byte
	pos       int
	nodes     int
	infoStart int
	infoEnd   int
	infoFound bool
}

func validCanonicalInt(text string) bool {
	if text == "0" {
		return true
	}
	s := text
	if strings.HasPrefix(s, "-") {
		s = s[1:]
		if s == "" {
			return false
		}
	}
	if s[0] < '1' || s[0] > '9' || len(s) > 20 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func (d *decoder) parse(depth int) (any, error) {
	d.nodes++
	if depth > maxDecoderDepth || d.nodes > maxDecoderNodes || d.pos >= len(d.data) {
		return nil, ErrInvalidMetainfo
	}
	switch token := d.data[d.pos]; {
	case token == 'i':
		rel := bytes.IndexByte(d.data[d.pos+1:], 'e')
		if rel < 0 {
			return nil, ErrInvalidMetainfo
		}
		text := string(d.data[d.pos+1 : d.pos+1+rel])
		if !validCanonicalInt(text) {
			return nil, fmt.Errorf("%w: non-canonical integer", ErrInvalidMetainfo)
		}
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: non-canonical integer", ErrInvalidMetainfo)
		}
		d.pos += rel + 2
		return value, nil
	case token == 'd' || token == 'l':
		d.pos++
		if token == 'l' {
			var list []any
			for d.pos < len(d.data) && d.data[d.pos] != 'e' {
				item, err := d.parse(depth + 1)
				if err != nil {
					return nil, err
				}
				list = append(list, item)
			}
			if d.pos >= len(d.data) {
				return nil, ErrInvalidMetainfo
			}
			d.pos++
			return list, nil
		}
		dict := &bDict{values: map[string]any{}}
		var previous []byte
		for d.pos < len(d.data) && d.data[d.pos] != 'e' {
			rawKey, err := d.parse(depth + 1)
			if err != nil {
				return nil, err
			}
			key, ok := rawKey.([]byte)
			if !ok {
				return nil, ErrInvalidMetainfo
			}
			if previous != nil && bytes.Compare(key, previous) <= 0 {
				return nil, fmt.Errorf("%w: unordered dictionary key", ErrInvalidMetainfo)
			}
			previous = key
			valueStart := d.pos
			value, err := d.parse(depth + 1)
			if err != nil {
				return nil, err
			}
			if depth == 0 && string(key) == "info" {
				d.infoStart, d.infoEnd, d.infoFound = valueStart, d.pos, true
			}
			dict.keys = append(dict.keys, string(key))
			dict.values[string(key)] = value
		}
		if d.pos >= len(d.data) {
			return nil, ErrInvalidMetainfo
		}
		d.pos++
		return dict, nil
	default:
		if token < '0' || token > '9' {
			return nil, ErrInvalidMetainfo
		}
		limit := d.pos + 12
		if limit > len(d.data) {
			limit = len(d.data)
		}
		colon := bytes.IndexByte(d.data[d.pos:limit], ':')
		if colon < 0 {
			return nil, ErrInvalidMetainfo
		}
		text := string(d.data[d.pos : d.pos+colon])
		if text != "0" && (text == "" || text[0] == '0') {
			return nil, fmt.Errorf("%w: bad string length", ErrInvalidMetainfo)
		}
		length, err := strconv.Atoi(text)
		if err != nil || length < 0 {
			return nil, fmt.Errorf("%w: bad string length", ErrInvalidMetainfo)
		}
		start := d.pos + colon + 1
		end := start + length
		if end > len(d.data) {
			return nil, ErrInvalidMetainfo
		}
		value := d.data[start:end]
		d.pos = end
		return value, nil
	}
}

var reservedWindowsNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// validateComponent mirrors musicops.clients.metainfo._component: a single
// path component must be valid UTF-8, safe on Windows and not reserved.
func validateComponent(raw []byte) (string, error) {
	if !utf8.Valid(raw) {
		return "", fmt.Errorf("%w: path is not valid UTF-8", ErrInvalidMetainfo)
	}
	name := string(raw)
	if name == "" || name == "." || name == ".." ||
		strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return "", fmt.Errorf("%w: unsafe path component", ErrInvalidMetainfo)
	}
	if strings.ContainsAny(name, "<>:\"/\\|?*") {
		return "", fmt.Errorf("%w: unsafe path component", ErrInvalidMetainfo)
	}
	for _, r := range name {
		if r < 0x20 {
			return "", fmt.Errorf("%w: unsafe path component", ErrInvalidMetainfo)
		}
	}
	stem := name
	if i := strings.IndexByte(name, '.'); i >= 0 {
		stem = name[:i]
	}
	if reservedWindowsNames[strings.ToUpper(stem)] {
		return "", fmt.Errorf("%w: reserved path component", ErrInvalidMetainfo)
	}
	return name, nil
}

func metainfoFile(path []string, raw *bDict) (MetainfoFile, error) {
	attr, hasAttr := dictBytes(raw, "attr")
	if raw.has("attr") && !hasAttr {
		return MetainfoFile{}, ErrInvalidMetainfo
	}
	if bytes.IndexByte(attr, 'l') >= 0 || raw.has("symlink path") {
		return MetainfoFile{}, fmt.Errorf("%w: symlink torrent blocked", ErrInvalidMetainfo)
	}
	length, ok := dictInt(raw, "length")
	if !ok || length < 0 {
		return MetainfoFile{}, ErrInvalidMetainfo
	}
	return MetainfoFile{
		Path:    strings.Join(path, "/"),
		Length:  length,
		Padding: bytes.IndexByte(attr, 'p') >= 0,
	}, nil
}

func v1Files(info *bDict, name string) ([]MetainfoFile, error) {
	if filesRaw, ok := info.get("files"); ok {
		if info.has("length") {
			return nil, ErrInvalidMetainfo
		}
		list, ok := filesRaw.([]any)
		if !ok || len(list) == 0 {
			return nil, ErrInvalidMetainfo
		}
		var out []MetainfoFile
		for _, itemRaw := range list {
			item, ok := itemRaw.(*bDict)
			if !ok {
				return nil, ErrInvalidMetainfo
			}
			pathRaw, ok := dictList(item, "path")
			if !ok || len(pathRaw) == 0 {
				return nil, ErrInvalidMetainfo
			}
			components := []string{name}
			for _, componentRaw := range pathRaw {
				raw, ok := componentRaw.([]byte)
				if !ok {
					return nil, ErrInvalidMetainfo
				}
				component, err := validateComponent(raw)
				if err != nil {
					return nil, err
				}
				components = append(components, component)
			}
			file, err := metainfoFile(components, item)
			if err != nil {
				return nil, err
			}
			out = append(out, file)
		}
		return out, nil
	}
	file, err := metainfoFile([]string{name}, info)
	if err != nil {
		return nil, err
	}
	return []MetainfoFile{file}, nil
}

func verifyPieceLayer(root, layer []byte, pieceLength, fileLength int64) error {
	count := (fileLength + pieceLength - 1) / pieceLength
	hashes := make([][]byte, 0, count)
	for i := int64(0); i < count; i++ {
		hashes = append(hashes, layer[i*32:(i+1)*32])
	}
	padding := make([]byte, 32)
	// (piece_length // 16384).bit_length() - 1 rounds of padding hashing.
	for i := 0; i < bits.Len64(uint64(pieceLength/16384))-1; i++ {
		sum := sha256.Sum256(append(append([]byte{}, padding...), padding...))
		padding = sum[:]
	}
	target := 1
	for target < len(hashes) {
		target <<= 1
	}
	for len(hashes) < target {
		hashes = append(hashes, padding)
	}
	for len(hashes) > 1 {
		next := make([][]byte, 0, len(hashes)/2)
		for i := 0; i < len(hashes); i += 2 {
			sum := sha256.Sum256(append(append([]byte{}, hashes[i]...), hashes[i+1]...))
			next = append(next, sum[:])
		}
		hashes = next
	}
	if !bytes.Equal(hashes[0], root) {
		return fmt.Errorf("%w: piece layer root mismatch", ErrInvalidMetainfo)
	}
	return nil
}

func v2Files(tree *bDict, pieceLength int64, layers map[string][]byte) ([]MetainfoFile, error) {
	if tree == nil || len(tree.keys) == 0 {
		return nil, ErrInvalidMetainfo
	}
	var files []MetainfoFile
	referenced := map[string]bool{}
	var walk func(node *bDict, path []string) error
	walk = func(node *bDict, path []string) error {
		if node == nil {
			return ErrInvalidMetainfo
		}
		if leafRaw, ok := node.get(""); ok {
			if len(node.keys) != 1 || len(path) == 0 {
				return ErrInvalidMetainfo
			}
			leaf, ok := leafRaw.(*bDict)
			if !ok {
				return ErrInvalidMetainfo
			}
			file, err := metainfoFile(path, leaf)
			if err != nil {
				return err
			}
			if file.Length > 0 {
				root, ok := dictBytes(leaf, "pieces root")
				if !ok || len(root) != 32 {
					return ErrInvalidMetainfo
				}
				file.PiecesRoot = hex.EncodeToString(root)
				if file.Length > pieceLength {
					layer, ok := layers[string(root)]
					count := (file.Length + pieceLength - 1) / pieceLength
					if !ok || int64(len(layer)) != count*32 {
						return ErrInvalidMetainfo
					}
					if err := verifyPieceLayer(root, layer, pieceLength, file.Length); err != nil {
						return err
					}
					referenced[string(root)] = true
				}
			}
			files = append(files, file)
			if len(files) > maxMetainfoFiles {
				return ErrInvalidMetainfo
			}
			return nil
		}
		for _, key := range node.keys {
			component, err := validateComponent([]byte(key))
			if err != nil {
				return err
			}
			childRaw, _ := node.get(key)
			child, ok := childRaw.(*bDict)
			if !ok {
				return ErrInvalidMetainfo
			}
			next := make([]string, len(path)+1)
			copy(next, path)
			next[len(path)] = component
			if err := walk(child, next); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(tree, nil); err != nil {
		return nil, err
	}
	if len(referenced) != len(layers) {
		return nil, fmt.Errorf("%w: unexpected piece layers", ErrInvalidMetainfo)
	}
	return files, nil
}

type filePair struct {
	Path   string
	Length int64
}

func nonPaddingPairs(files []MetainfoFile) []filePair {
	var out []filePair
	for _, f := range files {
		if !f.Padding {
			out = append(out, filePair{Path: f.Path, Length: f.Length})
		}
	}
	return out
}

func pairsEqual(a, b []filePair) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ParseMetainfo parses and validates a .torrent file, mirroring
// musicops.clients.metainfo.parse_metainfo. It returns the v1 SHA-1 and/or
// v2 SHA-256 infohash of the raw info dictionary bytes, the file list and
// the SHA-256 of the whole metainfo.
func ParseMetainfo(data []byte) (Metainfo, error) {
	if len(data) == 0 || len(data) > maxMetainfoBytes {
		return Metainfo{}, ErrInvalidMetainfo
	}
	dec := &decoder{data: data}
	rootRaw, err := dec.parse(0)
	if err != nil {
		return Metainfo{}, err
	}
	if dec.pos != len(data) {
		return Metainfo{}, ErrInvalidMetainfo
	}
	root, ok := rootRaw.(*bDict)
	if !ok {
		return Metainfo{}, ErrInvalidMetainfo
	}
	infoRaw, ok := root.get("info")
	if !ok || !dec.infoFound {
		return Metainfo{}, ErrInvalidMetainfo
	}
	info, ok := infoRaw.(*bDict)
	if !ok {
		return Metainfo{}, ErrInvalidMetainfo
	}
	metaVersion := int64(0)
	if v, ok := dictInt(info, "meta version"); ok {
		if v != 2 {
			return Metainfo{}, fmt.Errorf("%w: unsupported metainfo version", ErrInvalidMetainfo)
		}
		metaVersion = v
	} else if info.has("meta version") {
		return Metainfo{}, ErrInvalidMetainfo
	}
	nameRaw, ok := dictBytes(info, "name")
	if !ok {
		return Metainfo{}, ErrInvalidMetainfo
	}
	name, err := validateComponent(nameRaw)
	if err != nil {
		return Metainfo{}, err
	}
	pieceLength, ok := dictInt(info, "piece length")
	if !ok || pieceLength < 16384 || pieceLength&(pieceLength-1) != 0 {
		return Metainfo{}, fmt.Errorf("%w: invalid piece length", ErrInvalidMetainfo)
	}
	isV1 := info.has("pieces")
	isV2 := metaVersion == 2
	if !isV1 && !isV2 {
		return Metainfo{}, fmt.Errorf("%w: no supported metainfo identity", ErrInvalidMetainfo)
	}
	var filesV1, filesV2 []MetainfoFile
	if isV1 {
		filesV1, err = v1Files(info, name)
		if err != nil {
			return Metainfo{}, err
		}
		pieces, ok := dictBytes(info, "pieces")
		if !ok {
			return Metainfo{}, ErrInvalidMetainfo
		}
		var total int64
		for _, f := range filesV1 {
			total += f.Length
		}
		count := (total + pieceLength - 1) / pieceLength
		if int64(len(pieces)) != count*20 {
			return Metainfo{}, fmt.Errorf("%w: v1 piece count mismatch", ErrInvalidMetainfo)
		}
	}
	layers := map[string][]byte{}
	if isV2 {
		layersRaw, ok := dictDict(root, "piece layers")
		if !ok {
			return Metainfo{}, fmt.Errorf("%w: v2 missing piece layers", ErrInvalidMetainfo)
		}
		for _, key := range layersRaw.keys {
			layer, ok := dictBytes(layersRaw, key)
			if !ok {
				return Metainfo{}, ErrInvalidMetainfo
			}
			layers[key] = layer
		}
		tree, ok := dictDict(info, "file tree")
		if !ok {
			return Metainfo{}, fmt.Errorf("%w: v2 empty file tree", ErrInvalidMetainfo)
		}
		filesV2, err = v2Files(tree, pieceLength, layers)
		if err != nil {
			return Metainfo{}, err
		}
		if len(filesV2) == 0 {
			return Metainfo{}, fmt.Errorf("%w: v2 empty file tree", ErrInvalidMetainfo)
		}
	}
	if isV1 && isV2 {
		flat := nonPaddingPairs(filesV2)
		rooted := make([]filePair, len(flat))
		for i, p := range flat {
			rooted[i] = filePair{Path: name + "/" + p.Path, Length: p.Length}
		}
		if !pairsEqual(nonPaddingPairs(filesV1), flat) && !pairsEqual(nonPaddingPairs(filesV1), rooted) {
			return Metainfo{}, fmt.Errorf("%w: hybrid file manifests disagree", ErrInvalidMetainfo)
		}
	}
	files := filesV1
	if !isV1 {
		files = filesV2
	}
	if len(files) == 0 || len(files) > maxMetainfoFiles {
		return Metainfo{}, ErrInvalidMetainfo
	}
	seen := map[string]bool{}
	for _, f := range files {
		key := strings.ToLower(f.Path)
		if seen[key] {
			return Metainfo{}, fmt.Errorf("%w: duplicate path", ErrInvalidMetainfo)
		}
		seen[key] = true
	}
	for _, f := range files {
		parts := strings.Split(f.Path, "/")
		for i := 1; i < len(parts); i++ {
			if seen[strings.ToLower(strings.Join(parts[:i], "/"))] {
				return Metainfo{}, fmt.Errorf("%w: file/directory path collision", ErrInvalidMetainfo)
			}
		}
	}
	var totalSize int64
	for _, f := range files {
		totalSize += f.Length
		if totalSize < 0 {
			return Metainfo{}, ErrInvalidMetainfo
		}
	}
	meta := Metainfo{
		Name:        name,
		Files:       files,
		PieceLength: pieceLength,
		TotalSize:   totalSize,
	}
	switch {
	case isV1 && isV2:
		meta.Type = "hybrid"
	case isV2:
		meta.Type = "v2"
	default:
		meta.Type = "v1"
	}
	infoBytes := data[dec.infoStart:dec.infoEnd]
	if isV1 {
		sum := sha1.Sum(infoBytes)
		meta.InfoHashV1 = hex.EncodeToString(sum[:])
	}
	if isV2 {
		sum := sha256.Sum256(infoBytes)
		meta.InfoHashV2 = hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(data)
	meta.MetainfoSHA256 = hex.EncodeToString(sum[:])
	return meta, nil
}
