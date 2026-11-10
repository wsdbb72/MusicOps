package app

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strconv"
	"strings"
)

// xlsxToTable reads the first worksheet of a macro-free workbook into a
// tab-separated table. It does not run formulas or embedded content.
func xlsxToTable(data []byte) (string, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", errors.New("xlsx 无法打开")
	}
	files := map[string]*zip.File{}
	for _, file := range reader.File {
		if file.Name == "xl/vbaProject.bin" {
			return "", errors.New("不读取带宏的 xlsx")
		}
		files[file.Name] = file
	}
	shared, err := readSharedStrings(files["xl/sharedStrings.xml"])
	if err != nil {
		return "", err
	}
	sheet := files["xl/worksheets/sheet1.xml"]
	if sheet == nil {
		return "", errors.New("xlsx 没有第一张表")
	}
	raw, err := readZip(sheet)
	if err != nil {
		return "", err
	}
	rows := map[int]map[int]string{}
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	var cellRef, cellType string
	var inValue bool
	var value strings.Builder
	maxRow := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", errors.New("xlsx 表格无法解析")
		}
		switch node := token.(type) {
		case xml.StartElement:
			if node.Name.Local == "c" {
				cellRef, cellType = "", ""
				for _, attr := range node.Attr {
					if attr.Name.Local == "r" {
						cellRef = attr.Value
					}
					if attr.Name.Local == "t" {
						cellType = attr.Value
					}
				}
			}
			inValue = node.Name.Local == "v" || node.Name.Local == "t"
			value.Reset()
		case xml.CharData:
			if inValue {
				value.Write(node)
			}
		case xml.EndElement:
			if node.Name.Local == "v" || (node.Name.Local == "t" && cellType == "inlineStr") {
				row, col := cellPosition(cellRef)
				if row > 0 && col > 0 {
					text := value.String()
					if cellType == "s" {
						index, _ := strconv.Atoi(text)
						if index >= 0 && index < len(shared) {
							text = shared[index]
						}
					}
					if rows[row] == nil {
						rows[row] = map[int]string{}
					}
					rows[row][col] = text
					if row > maxRow {
						maxRow = row
					}
				}
			}
			if node.Name.Local == "v" || node.Name.Local == "t" {
				inValue = false
			}
		}
	}
	var out strings.Builder
	for row := 1; row <= maxRow; row++ {
		cells := rows[row]
		width := 0
		for col := range cells {
			if col > width {
				width = col
			}
		}
		var line []string
		for col := 1; col <= width; col++ {
			line = append(line, cells[col])
		}
		out.WriteString(strings.Join(line, "\t"))
		out.WriteByte('\n')
	}
	return out.String(), nil
}

func readSharedStrings(file *zip.File) ([]string, error) {
	if file == nil {
		return nil, nil
	}
	raw, err := readZip(file)
	if err != nil {
		return nil, err
	}
	var stringsOut []string
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	var inText bool
	var text strings.Builder
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("xlsx 共享字符串无法解析")
		}
		switch node := token.(type) {
		case xml.StartElement:
			if node.Name.Local == "t" {
				inText = true
				text.Reset()
			}
		case xml.CharData:
			if inText {
				text.Write(node)
			}
		case xml.EndElement:
			if node.Name.Local == "t" && inText {
				stringsOut = append(stringsOut, text.String())
				inText = false
			}
		}
	}
	return stringsOut, nil
}

func readZip(file *zip.File) ([]byte, error) {
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(io.LimitReader(reader, 20<<20))
}

func cellPosition(ref string) (int, int) {
	col := 0
	row := 0
	for _, r := range ref {
		switch {
		case r >= 'A' && r <= 'Z':
			col = col*26 + int(r-'A'+1)
		case r >= '0' && r <= '9':
			row = row*10 + int(r-'0')
		}
	}
	return row, col
}
