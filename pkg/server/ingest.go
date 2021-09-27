package server

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/pyroscope-io/pyroscope/pkg/agent/types"
	"github.com/pyroscope-io/pyroscope/pkg/convert"
	"github.com/pyroscope-io/pyroscope/pkg/storage"
	"github.com/pyroscope-io/pyroscope/pkg/storage/segment"
	"github.com/pyroscope-io/pyroscope/pkg/storage/tree"
	"github.com/pyroscope-io/pyroscope/pkg/util/attime"
)

type ingestParams struct {
	parserFunc      parserFunc
	storageKey      *segment.Key
	spyName         string
	sampleRate      uint32
	units           string
	aggregationType string
	modifiers       []string
	from            time.Time
	until           time.Time
}

type convertFunc func(r io.Reader, cb func([]byte, int)) error
type convertFuncBuf func(r io.Reader, tmpBuf []byte, cb func([]byte, int)) error
type convertFuncReader func(r io.Reader) (*tree.Tree, error)

type parserFunc func(io.Reader, []byte) (*tree.Tree, error)

func wrapConvertFunction(f convertFunc) parserFunc {
	return func(r io.Reader, _ []byte) (*tree.Tree, error) {
		t := tree.New()
		return t, f(r, treeInsert(t))
	}
}

func wrapConvertFunctionBuf(f convertFuncBuf) parserFunc {
	return func(r io.Reader, tmpBuf []byte) (*tree.Tree, error) {
		t := tree.New()
		return t, f(r, tmpBuf, treeInsert(t))
	}
}

func treeInsert(t *tree.Tree) func([]byte, int) {
	return func(k []byte, v int) { t.Insert(k, uint64(v)) }
}

func wrapConvertFunctionReader(f convertFuncReader) parserFunc {
	return func(r io.Reader, _ []byte) (*tree.Tree, error) { return f(r) }
}

func (ctrl *Controller) ingestHandler(w http.ResponseWriter, r *http.Request) {
	var ip ingestParams
	if err := ctrl.ingestParamsFromRequest(r, &ip); err != nil {
		ctrl.writeInvalidParameterError(w, err)
		return
	}

	var t *tree.Tree
	tmpBuf := ctrl.bufferPool.Get()
	t, err := ip.parserFunc(r.Body, tmpBuf.B)
	ctrl.bufferPool.Put(tmpBuf)

	if err != nil {
		ctrl.writeError(w, http.StatusUnprocessableEntity, err, "error happened while parsing request body")
		return
	}

	err = ctrl.ingester.Put(&storage.PutInput{
		StartTime:       ip.from,
		EndTime:         ip.until,
		Key:             ip.storageKey,
		Val:             t,
		SpyName:         ip.spyName,
		SampleRate:      ip.sampleRate,
		Units:           ip.units,
		AggregationType: ip.aggregationType,
	})
	if err != nil {
		ctrl.writeInternalServerError(w, err, "error happened while ingesting data")
		return
	}

	ctrl.statsInc("ingest")
	ctrl.statsInc("ingest:" + ip.spyName)
	k := *ip.storageKey
	ctrl.appStats.Add(hashString(k.AppName()))
}

func (ctrl *Controller) ingestParamsFromRequest(r *http.Request, ip *ingestParams) error {
	q := r.URL.Query()
	format := q.Get("format")
	contentType := r.Header.Get("Content-Type")
	switch {
	case format == "tree", contentType == "binary/octet-stream+tree":
		ip.parserFunc = wrapConvertFunctionReader(tree.DeserializeV1NoDict)
	case format == "trie", contentType == "binary/octet-stream+trie":
		ip.parserFunc = wrapConvertFunctionBuf(convert.ParseTrieBuf)
	case format == "lines":
		ip.parserFunc = wrapConvertFunction(convert.ParseIndividualLines)
	default:
		ip.parserFunc = wrapConvertFunction(convert.ParseGroups)
	}

	if qt := q.Get("from"); qt != "" {
		ip.from = attime.Parse(qt)
	} else {
		ip.from = time.Now()
	}

	if qt := q.Get("until"); qt != "" {
		ip.until = attime.Parse(qt)
	} else {
		ip.until = time.Now()
	}

	if sr := q.Get("sampleRate"); sr != "" {
		sampleRate, err := strconv.Atoi(sr)
		if err != nil {
			logrus.WithField("err", err).Errorf("invalid sample rate: %v", sr)
			ip.sampleRate = types.DefaultSampleRate
		} else {
			ip.sampleRate = uint32(sampleRate)
		}
	} else {
		ip.sampleRate = types.DefaultSampleRate
	}

	if sn := q.Get("spyName"); sn != "" {
		// TODO: error handling
		ip.spyName = sn
	} else {
		ip.spyName = "unknown"
	}

	if u := q.Get("units"); u != "" {
		ip.units = u
	} else {
		ip.units = "samples"
	}

	if at := q.Get("aggregationType"); at != "" {
		ip.aggregationType = at
	} else {
		ip.aggregationType = "sum"
	}

	var err error
	ip.storageKey, err = segment.ParseKey(q.Get("name"))
	if err != nil {
		return fmt.Errorf("name: %w", err)
	}
	return nil
}
