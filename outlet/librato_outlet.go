package outlet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"l2met/bucket"
	"l2met/utils"
	"net"
	"net/http"
	"strconv"
	"time"
)

var libratoUrl = "https://metrics-api.librato.com/v1/metrics"

var httpClient *http.Client

func init() {
	tr := &http.Transport{
		DisableKeepAlives: true,
		Dial: func(n, a string) (net.Conn, error) {
			c, err := net.DialTimeout(n, a, time.Second*2)
			if err != nil {
				return c, err
			}
			return c, c.SetDeadline(time.Now().Add(time.Second * 2))
		},
	}
	httpClient = &http.Client{Transport: tr}
}

type LibratoAttributes struct {
	Min   int    `json:"display_min"`
	Units string `json:"display_units_long"`
}

type Payload struct {
	Name   string             `json:"name"`
	Time   int64              `json:"measure_time"`
	Val    string             `json:"value,omitempty"`
	Source string             `json:"source,omitempty"`
	User   string             `json:"-"`
	Pass   string             `json:"-"`
	Attr   *LibratoAttributes `json:"attributes,omitempty"`
}

type LibratoRequest struct {
	Gauges []*Payload `json:"gauges"`
}

type LibratoOutlet struct {
	// The inbox is used to hold empty buckets that are
	// waiting to be processed. We buffer the chanel so
	// as not to slow down the fetch routine.
	Inbox chan *bucket.Bucket
	// The converter will take items from the inbox,
	// fill in the bucket with the vals, then convert the
	// bucket into a librato metric.
	Conversions chan *Payload
	// The converter will place the librato metrics into
	// the outbox for HTTP submission. We rely on the batch
	// routine to make sure that the collections of librato metrics
	// in the outbox are homogeneous with respect to their token.
	// This ensures that we route the metrics to the correct librato account.
	Outbox chan []*Payload
	// How many outlet routines should be running.
	NumOutlets int
	// How many accept routines should be running.
	NumConverters int
	//We use the Reader to read buckets from the store into our Inbox.
	Reader Reader
	//Number of times to retry HTTP requests to librato's api.
	Retries int
}

func NewLibratoOutlet(s, c, r int, rdr Reader) *LibratoOutlet {
	l := new(LibratoOutlet)
	l.Inbox = make(chan *bucket.Bucket, s)
	l.Conversions = make(chan *Payload, s)
	l.Outbox = make(chan []*Payload, s)
	l.NumConverters = c
	l.NumOutlets = c
	l.Reader = rdr
	return l
}

func (l *LibratoOutlet) Start() {
	go l.Reader.Start(l.Inbox)
	for i := 0; i < l.NumConverters; i++ {
		go l.convert()
	}
	go l.batch()
	for i := 0; i < l.NumOutlets; i++ {
		go l.outlet()
	}
}

func (l *LibratoOutlet) Report() {
	for _ = range time.Tick(time.Second * 2) {
		utils.MeasureI("librato-outlet.inbox", "buckets", int64(len(l.Inbox)))
		utils.MeasureI("librato-outlet.conversions", "payloads", int64(len(l.Conversions)))
		utils.MeasureI("librato-outlet.outbox", "requests", int64(len(l.Outbox)))
	}
}

func (l *LibratoOutlet) convert() {
	for bucket := range l.Inbox {
		if len(bucket.Vals) == 0 {
			fmt.Printf("at=bucket-no-vals bucket=%s\n", bucket.Id.Name)
			continue
		}
		attrs := &LibratoAttributes{Min: 0, Units: bucket.Id.Units}
		//TODO(ryandotsmith): Some day Librato will support these
		//metrics in their complex measurement api. We will need to
		//move these up ^^ into the complex payload.
//		l.Conversions <- &Payload{Attr: attrs, User: bucket.Id.User, Pass: bucket.Id.Pass, Time: ft(bucket.Id.Time), Source: bucket.Id.Source, Name: bucket.Id.Name + ".min", Val: ff(bucket.Min())}
//		l.Conversions <- &Payload{Attr: attrs, User: bucket.Id.User, Pass: bucket.Id.Pass, Time: ft(bucket.Id.Time), Source: bucket.Id.Source, Name: bucket.Id.Name + ".max", Val: ff(bucket.Max())}
//		l.Conversions <- &Payload{Attr: attrs, User: bucket.Id.User, Pass: bucket.Id.Pass, Time: ft(bucket.Id.Time), Source: bucket.Id.Source, Name: bucket.Id.Name + ".sum", Val: ff(bucket.Sum())}
//		l.Conversions <- &Payload{Attr: attrs, User: bucket.Id.User, Pass: bucket.Id.Pass, Time: ft(bucket.Id.Time), Source: bucket.Id.Source, Name: bucket.Id.Name + ".count", Val: fi(bucket.Count())}
		l.Conversions <- &Payload{Attr: attrs, User: bucket.Id.User, Pass: bucket.Id.Pass, Time: ft(bucket.Id.Time), Source: bucket.Id.Source, Name: bucket.Id.Name + ".mean", Val: ff(bucket.Mean())}
//		l.Conversions <- &Payload{Attr: attrs, User: bucket.Id.User, Pass: bucket.Id.Pass, Time: ft(bucket.Id.Time), Source: bucket.Id.Source, Name: bucket.Id.Name + ".last", Val: ff(bucket.Last())}
//		l.Conversions <- &Payload{Attr: attrs, User: bucket.Id.User, Pass: bucket.Id.Pass, Time: ft(bucket.Id.Time), Source: bucket.Id.Source, Name: bucket.Id.Name + ".median", Val: ff(bucket.Median())}
//		l.Conversions <- &Payload{Attr: attrs, User: bucket.Id.User, Pass: bucket.Id.Pass, Time: ft(bucket.Id.Time), Source: bucket.Id.Source, Name: bucket.Id.Name + ".perc95", Val: ff(bucket.P95())}
//		l.Conversions <- &Payload{Attr: attrs, User: bucket.Id.User, Pass: bucket.Id.Pass, Time: ft(bucket.Id.Time), Source: bucket.Id.Source, Name: bucket.Id.Name + ".perc99", Val: ff(bucket.P99())}
		fmt.Printf("measure.bucket.conversion.delay=%d\n", bucket.Id.Delay(time.Now()))
	}
}

func (l *LibratoOutlet) batch() {
	ticker := time.Tick(time.Millisecond * 200)
	batchMap := make(map[string][]*Payload)
	for {
		select {
		case <-ticker:
			for k, v := range batchMap {
				if len(v) > 0 {
					l.Outbox <- v
				}
				delete(batchMap, k)
			}
		case payload := <-l.Conversions:
			index := payload.User + ":" + payload.Pass
			_, present := batchMap[index]
			if !present {
				batchMap[index] = make([]*Payload, 1, 300)
				batchMap[index][0] = payload
			} else {
				batchMap[index] = append(batchMap[index], payload)
			}
			if len(batchMap[index]) == cap(batchMap[index]) {
				l.Outbox <- batchMap[index]
				delete(batchMap, index)
			}
		}
	}
}

func (l *LibratoOutlet) outlet() {
	for payloads := range l.Outbox {
		if len(payloads) < 1 {
			fmt.Printf("at=%q\n", "empty-metrics-error")
			continue
		}
		//Since a playload contains all metrics for
		//a unique librato user/pass, we can extract the user/pass
		//from any one of the payloads.
		sample := payloads[0]
		reqBody := new(LibratoRequest)
		reqBody.Gauges = payloads
		j, err := json.Marshal(reqBody)
		if err != nil {
			fmt.Printf("at=json-error error=%s user=%s\n", err, sample.User)
			continue
		}
		l.postWithRetry(sample.User, sample.Pass, bytes.NewBuffer(j))
	}
}

func (l *LibratoOutlet) postWithRetry(u, p string, body *bytes.Buffer) error {
	for i := 0; i <= l.Retries; i++ {
		if err := l.post(u, p, body); err != nil {
			fmt.Printf("measure.librato.error user=%s msg=%s attempt=%d\n", u, err, i)
			if i == l.Retries {
				return err
			}
			continue
		}
		return nil
	}
	//Should not be possible.
	return errors.New("Unable to post.")
}

func (l *LibratoOutlet) post(u, p string, body *bytes.Buffer) error {
	defer utils.MeasureT("librato-post", time.Now())
	req, err := http.NewRequest("POST", libratoUrl, body)
	if err != nil {
		return err
	}
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("User-Agent", "l2met/0")
	req.SetBasicAuth(u, p)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var m string
		s, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			m = fmt.Sprintf("error=failed-request code=%d", resp.StatusCode)
		} else {
			m = fmt.Sprintf("error=failed-request code=%d resp=body=%s req-body=%s",
				resp.StatusCode, s, body)
		}
		return errors.New(m)
	}
	return nil
}

func ff(x float64) string {
	return strconv.FormatFloat(x, 'f', 5, 64)
}

func fi(x int) string {
	return strconv.FormatInt(int64(x), 10)
}

func ft(t time.Time) int64 {
	return t.Unix()
}
