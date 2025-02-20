// Copyright (c) 2015-2021 MinIO, Inc.
//
// This file is part of MinIO Object Storage stack
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package cmd

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"
	"golang.org/x/text/encoding/korean"
        "golang.org/x/text/transform"
	// golang does not support flat keys for path matching, find does

	"github.com/minio/mc/pkg/probe"
	"github.com/minio/minio-go/v7"
	"golang.org/x/text/unicode/norm"
)

// differType difference in type.
type differType int

const (
	differInUnknown       differType = iota
	differInNone                     // does not differ
	differInSize                     // differs in size
	differInMetadata                 // differs in metadata
	differInType                     // differs in type, exfile/directory
	differInFirst                    // only in source (FIRST)
	differInSecond                   // only in target (SECOND)
	differInAASourceMTime            // differs in active-active source modtime
)

func (d differType) String() string {
	switch d {
	case differInNone:
		return ""
	case differInSize:
		return "size"
	case differInMetadata:
		return "metadata"
	case differInAASourceMTime:
		return "mm-source-mtime"
	case differInType:
		return "type"
	case differInFirst:
		return "only-in-first"
	case differInSecond:
		return "only-in-second"
	}
	return "unknown"
}

const activeActiveSourceModTimeKey = "X-Amz-Meta-Mm-Source-Mtime"

func getSourceModTimeKey(metadata map[string]string) string {
	if metadata[activeActiveSourceModTimeKey] != "" {
		return metadata[activeActiveSourceModTimeKey]
	}
	if metadata[strings.ToLower(activeActiveSourceModTimeKey)] != "" {
		return metadata[strings.ToLower(activeActiveSourceModTimeKey)]
	}
	if metadata[strings.ToLower("Mm-Source-Mtime")] != "" {
		return metadata[strings.ToLower("Mm-Source-Mtime")]
	}
	if metadata["Mm-Source-Mtime"] != "" {
		return metadata["Mm-Source-Mtime"]
	}
	return ""
}

// activeActiveModTimeUpdated tries to calculate if the object copy in the target
// is older than the one in the source by comparing the modtime of the data.
func activeActiveModTimeUpdated(src, dst *ClientContent) bool {
	if src == nil || dst == nil {
		return false
	}

	if src.Time.IsZero() || dst.Time.IsZero() {
		// This should only happen in a messy environment
		// but we are returning false anyway so the caller
		// function won't take any action.
		return false
	}

	srcActualModTime := src.Time
	dstActualModTime := dst.Time

	srcModTime := getSourceModTimeKey(src.UserMetadata)
	dstModTime := getSourceModTimeKey(dst.UserMetadata)
	if srcModTime == "" && dstModTime == "" {
		// No active-active mirror context found, fallback to modTimes presented
		// by the client content
		return srcActualModTime.After(dstActualModTime)
	}

	var srcOriginLastModified, dstOriginLastModified time.Time
	var err error
	if srcModTime != "" {
		srcOriginLastModified, err = time.Parse(time.RFC3339Nano, srcModTime)
		if err != nil {
			// failure to parse source modTime, modTime tampered ignore the file
			return false
		}
	}
	if dstModTime != "" {
		dstOriginLastModified, err = time.Parse(time.RFC3339Nano, dstModTime)
		if err != nil {
			// failure to parse source modTime, modTime tampered ignore the file
			return false
		}
	}

	if !srcOriginLastModified.IsZero() && srcOriginLastModified.After(src.Time) {
		srcActualModTime = srcOriginLastModified
	}

	if !dstOriginLastModified.IsZero() && dstOriginLastModified.After(dst.Time) {
		dstActualModTime = dstOriginLastModified
	}

	return srcActualModTime.After(dstActualModTime)
}

func metadataEqual(m1, m2 map[string]string) bool {
	for k, v := range m1 {
		if k == activeActiveSourceModTimeKey {
			continue
		}
		if k == strings.ToLower(activeActiveSourceModTimeKey) {
			continue
		}
		if m2[k] != v {
			return false
		}
	}
	for k, v := range m2 {
		if k == activeActiveSourceModTimeKey {
			continue
		}
		if k == strings.ToLower(activeActiveSourceModTimeKey) {
			continue
		}
		if m1[k] != v {
			return false
		}
	}
	return true
}

func objectDifference(ctx context.Context, sourceClnt, targetClnt Client, isMetadata bool) (diffCh chan diffMessage) {
	return difference(ctx, sourceClnt, targetClnt, isMetadata, true, false, DirNone)
}

func dirDifference(ctx context.Context, sourceClnt, targetClnt Client) (diffCh chan diffMessage) {
	return difference(ctx, sourceClnt, targetClnt, false, false, true, DirFirst)
}

func isCP949(s string) bool {
        ret := false
        sz := len(s)
        for i := 0 ; i < sz ; i++ {
                si := s[i]
                if( si >= 0x20 && si <=0x7E ) {
                        continue
                }
                if i+1 < sz {
			si2 := s[i+1]

			if( ((si >= 0xA1 && si <0xFE) && (si2 >= 0xA1 && si2 <=0xFE)) ||
			((si >= 0x81 && si <= 0xA0) && ((si2 >= 0x41 && si2 <= 0x5A) || (si2 >= 0x61 && si2 <= 0x7A) || (si2 >= 0x81 && si2 <= 0xFE))) ||
			((si >= 0xA1 && si <= 0xC5) && ((si2 >= 0x41 && si2 <= 0x5A) || (si2 >= 0x61 && si2 <= 0x7A) || (si2 >= 0x81 && si2 <= 0xA0))) ||
			((si == 0xC6) && ((si2 >= 0x41 && si2 <= 0x52)))){
				ret = true;
				i++
                        } else {
                                return false
                        }
                } else {
                        return false
                }
        }
        return ret
}

func iconvCP949ToUTF8(s string) string {
        if isCP949(s) {
                ret, n, err := transform.String(korean.EUCKR.NewDecoder(), s)
                if err != nil {
                        return s
                }

                if n >= 0 {
                        return ret
                }
        }

        return s
}

func differenceInternal(ctx context.Context, sourceClnt, targetClnt Client, isMetadata bool, isRecursive, returnSimilar bool, dirOpt DirOpt, diffCh chan<- diffMessage) *probe.Error {
	// Set default values for listing.
	srcCh := sourceClnt.List(ctx, ListOptions{Recursive: isRecursive, WithMetadata: isMetadata, ShowDir: dirOpt})
	tgtCh := targetClnt.List(ctx, ListOptions{Recursive: isRecursive, WithMetadata: isMetadata, ShowDir: dirOpt})

	srcCtnt, srcOk := <-srcCh
	tgtCtnt, tgtOk := <-tgtCh

	var srcEOF, tgtEOF bool

	for {
		srcEOF = !srcOk
		tgtEOF = !tgtOk

		// No objects from source AND target: Finish
		if srcEOF && tgtEOF {
			break
		}

		if !srcEOF && srcCtnt.Err != nil {
			return srcCtnt.Err.Trace(sourceClnt.GetURL().String(), targetClnt.GetURL().String())
		}

		if !tgtEOF && tgtCtnt.Err != nil {
			return tgtCtnt.Err.Trace(sourceClnt.GetURL().String(), targetClnt.GetURL().String())
		}

		// If source doesn't have objects anymore, comparison becomes obvious
		if srcEOF {
			diffCh <- diffMessage{
				SecondURL:     tgtCtnt.URL.String(),
				Diff:          differInSecond,
				secondContent: tgtCtnt,
			}
			tgtCtnt, tgtOk = <-tgtCh
			continue
		}

		// The same for target
		if tgtEOF {
			diffCh <- diffMessage{
				FirstURL:     srcCtnt.URL.String(),
				Diff:         differInFirst,
				firstContent: srcCtnt,
			}
			srcCtnt, srcOk = <-srcCh
			continue
		}

		srcSuffix := strings.TrimPrefix(srcCtnt.URL.String(), sourceClnt.GetURL().String())
		tgtSuffix := strings.TrimPrefix(tgtCtnt.URL.String(), targetClnt.GetURL().String())

		current := iconvCP949ToUTF8(urlJoinPath(targetClnt.GetURL().String(), srcSuffix))
		expected := urlJoinPath(targetClnt.GetURL().String(), tgtSuffix)

		if !utf8.ValidString(srcSuffix) && !isCP949(srcSuffix) {
			// Error. Keys must be valid UTF-8.
			diffCh <- diffMessage{Error: errInvalidSource(current).Trace()}
			srcCtnt, srcOk = <-srcCh
			continue
		}
		if !utf8.ValidString(tgtSuffix) && !isCP949(tgtSuffix) {
			// Error. Keys must be valid UTF-8.
			diffCh <- diffMessage{Error: errInvalidTarget(expected).Trace()}
			tgtCtnt, tgtOk = <-tgtCh
			continue
		}

		// Normalize to avoid situations where multiple byte representations are possible.
		// e.g. 'ä' can be represented as precomposed U+00E4 (UTF-8 0xc3a4) or decomposed
		// U+0061 U+0308 (UTF-8 0x61cc88).
		normalizedCurrent := norm.NFC.String(current)
		normalizedExpected := norm.NFC.String(expected)

		if normalizedExpected > normalizedCurrent {
			diffCh <- diffMessage{
				FirstURL:     srcCtnt.URL.String(),
				Diff:         differInFirst,
				firstContent: srcCtnt,
			}
			srcCtnt, srcOk = <-srcCh
			continue
		}
		if normalizedExpected == normalizedCurrent {
			srcType, tgtType := srcCtnt.Type, tgtCtnt.Type
			srcSize, tgtSize := srcCtnt.Size, tgtCtnt.Size
			if srcType.IsRegular() && !tgtType.IsRegular() ||
				!srcType.IsRegular() && tgtType.IsRegular() { 
				// Type differs. Source is never a directory.
				diffCh <- diffMessage{
					FirstURL:      srcCtnt.URL.String(),
					SecondURL:     tgtCtnt.URL.String(),
					Diff:          differInType,
					firstContent:  srcCtnt,
					secondContent: tgtCtnt,
				}
				continue
			}
			if srcSize != tgtSize {
				// Regular files differing in size.
				diffCh <- diffMessage{
					FirstURL:      srcCtnt.URL.String(),
					SecondURL:     tgtCtnt.URL.String(),
					Diff:          differInSize,
					firstContent:  srcCtnt,
					secondContent: tgtCtnt,
				}
			} else if activeActiveModTimeUpdated(srcCtnt, tgtCtnt) {
				diffCh <- diffMessage{
					FirstURL:      srcCtnt.URL.String(),
					SecondURL:     tgtCtnt.URL.String(),
					Diff:          differInAASourceMTime,
					firstContent:  srcCtnt,
					secondContent: tgtCtnt,
				}
			} else if isMetadata &&
				!metadataEqual(srcCtnt.UserMetadata, tgtCtnt.UserMetadata) &&
				!metadataEqual(srcCtnt.Metadata, tgtCtnt.Metadata) {

				// Regular files user requesting additional metadata to same file.
				diffCh <- diffMessage{
					FirstURL:      srcCtnt.URL.String(),
					SecondURL:     tgtCtnt.URL.String(),
					Diff:          differInMetadata,
					firstContent:  srcCtnt,
					secondContent: tgtCtnt,
				}
			}

			// No differ
			if returnSimilar {
				diffCh <- diffMessage{
					FirstURL:      srcCtnt.URL.String(),
					SecondURL:     tgtCtnt.URL.String(),
					Diff:          differInNone,
					firstContent:  srcCtnt,
					secondContent: tgtCtnt,
				}
			}
			srcCtnt, srcOk = <-srcCh
			tgtCtnt, tgtOk = <-tgtCh

			continue
		}
		// Differ in second
		diffCh <- diffMessage{
			SecondURL:     tgtCtnt.URL.String(),
			Diff:          differInSecond,
			secondContent: tgtCtnt,
		}
		tgtCtnt, tgtOk = <-tgtCh
		continue
	}

	return nil
}

// objectDifference function finds the difference between all objects
// recursively in sorted order from source and target.
func difference(ctx context.Context, sourceClnt, targetClnt Client, isMetadata bool, isRecursive, returnSimilar bool, dirOpt DirOpt) (diffCh chan diffMessage) {
	diffCh = make(chan diffMessage, 10000)

	go func() {
		defer close(diffCh)

		err := differenceInternal(ctx, sourceClnt, targetClnt, isMetadata, isRecursive, returnSimilar, dirOpt, diffCh)
		if err != nil {
			// handle this specifically for filesystem related errors.
			switch v := err.ToGoError().(type) {
			case PathNotFound, PathInsufficientPermission:
				diffCh <- diffMessage{
					Error: err,
				}
				return
			case minio.ErrorResponse:
				switch v.Code {
				case "NoSuchBucket", "NoSuchKey":
					diffCh <- diffMessage{
						Error: err,
					}
					return
				}
			}
			errorIf(err, "Unable to list comparison retrying..")
		}
	}()

	return diffCh
}
