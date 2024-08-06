//
// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// BufferedWriter is a writer to write table records in bulk.
//
// NOTE: BufferedWriter is not goroutine-safe.
type BufferedWriter struct {
	out      io.Writer
	table    *Table
	buffer   []string
	bulkSize uint
	format   string
}

// NewBufferedWriter creates BufferedWriter with specified configs.
func NewBufferedWriter(table *Table, out io.Writer, bulkSize uint, format string) *BufferedWriter {
	return &BufferedWriter{
		out:      out,
		table:    table,
		buffer:   make([]string, 0, bulkSize),
		bulkSize: bulkSize,
		format:   format,
	}
}

// Write writes a single record into the buffer. If buffer becomes full, it is flushed.
func (w *BufferedWriter) Write(values []string) {
	w.buffer = append(w.buffer, fmt.Sprintf("(%s)", strings.Join(values, ", ")))
	if len(w.buffer) >= int(w.bulkSize) {
		if w.format == "json" {
			w.FormatJson()
		} else if w.format == "sql" {
			w.FormatSql()
		}
	}
}

// Flush flushes the buffered records.
func (w *BufferedWriter) FormatSql() {
	if len(w.buffer) == 0 {
		return
	}

	quotedColumns := w.table.quotedColumnList()

	// Calculate the size of buffer for strings.Builder
	n := len(w.buffer) * 2 // 2 is for value separator (", ")
	n += len(quotedColumns)
	n += 100 // 100 is for remained statement ("INSERT INTO ...")
	for i := 0; i < len(w.buffer); i++ {
		n += len(w.buffer[i])
	}

	// Use strings.Builder to avoid string being copied to build INSERT statement
	sb := &strings.Builder{}
	sb.Grow(n)
	sb.WriteString("INSERT INTO `")
	sb.WriteString(w.table.Name)
	sb.WriteString("` (")
	sb.WriteString(quotedColumns)
	sb.WriteString(") VALUES ")
	for i, b := range w.buffer {
		sb.WriteString(b)
		if i < (len(w.buffer) - 1) {
			sb.WriteString(", ")
		}
	}
	sb.WriteString(";\n")

	fmt.Fprint(w.out, sb.String())
	w.buffer = w.buffer[:0]
}

// Flush flushes the buffered records.
func (w *BufferedWriter) FormatJson() {
	// Flush flushes the buffered records.
	if len(w.buffer) == 0 {
		return
	}

	columns := w.table.quotedColumnList()
	columnsList := strings.Split(columns, ", ")

	// For each buffered record, create a map where column names are keys
	for _, record := range w.buffer {
		values := strings.Split(record, ", ")
		rowData := make(map[string]string)
		for j, column := range columnsList {
			rowData[string(column)] = values[j]
		}

		// Convert the map to JSON
		jsonData, err := json.Marshal(rowData)
		if err != nil {
			fmt.Println(err)
			return
		}

		// Write the JSON data to the output, followed by a newline
		fmt.Fprintln(w.out, string(jsonData))
	}

	w.buffer = w.buffer[:0]
}
