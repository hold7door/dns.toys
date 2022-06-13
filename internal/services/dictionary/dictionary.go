package dictionary

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	apiUrl = "https://api.dictionaryapi.dev/api/v2/entries/en/%s"
 	apiRateLimit = 10
	apiRateLimitBurstSize = 1
)

type Opt struct {
	UserAgent  string
}

type Dictionary struct {
	data map[string]entry

	fetchQueue chan string

	limiter *rate.Limiter
	mut sync.RWMutex

	opt    Opt
	client *http.Client
}

type Meaning struct {
	PartOfSpeech string
	Definition string
}

type entry struct {
	Word string
	Meanings []Meaning
	Valid bool
	// when word not found
	// Valid = true and Found = false
	Found bool
	ExpiresAt time.Time
}

type wordData struct {
	Word string `json:"word"`
	Meanings []struct {
		PartOfSpeech string `json:"partOfSpeech"`
		Definitions []struct {
			Definition string `json:"definition"`
		} `json:"definitions"`
	} `json:"meanings"`
}

type wordNotFound struct {
	Title string `json:"title"`
}

var errQueued = errors.New("data is queued for fetch.")
var errNotFound = errors.New("the word was not found in dictionary.")


func New(o Opt) *Dictionary {
	d := &Dictionary{
		data: make(map[string]entry),
		fetchQueue: make(chan string),

		limiter: rate.NewLimiter(apiRateLimit, apiRateLimitBurstSize),
		opt:     o,
		client: &http.Client{
			Timeout: 0,
			Transport: &http.Transport{
				MaxIdleConnsPerHost:   10,
				ResponseHeaderTimeout: 0,
			},
		},
	}

	go d.runFetchQueue()

	return d
}

func (d *Dictionary) runFetchQueue() {
	for {
		select {
		case w := <- d.fetchQueue:
			if !d.limiter.Allow() {
				log.Println("dictionary api exceeded rate limit")
				continue
			}

			res, err := d.fetchAPI(w)

			// Even if it's an error, cache to avoid flooding the service.
			d.mut.Lock()
			d.data[w] = res
			d.mut.Unlock()

			if err != nil && err != errNotFound {
				log.Printf("error fetching dictionaryapi API: %v", err)
				continue
			}
		}
	}
}

func (d *Dictionary) Query(q string) ([]string, error){
	q = strings.ToLower(q)

	out, err := d.get(q)

	if err != nil {
		// Data never existed and has been queued. Show a friendly
		// message instead of an error.
		if err == errQueued {
			r := fmt.Sprintf("%s 1 TXT \"word definition is being fetched. Try again in a few seconds.\"", q)
			return []string{r}, nil
		}
		return nil, err
	}

	return out, nil

}

func (d *Dictionary) get(w string) ([]string, error) {
	d.mut.RLock()
	data, ok := d.data[w]
	d.mut.RUnlock()

	expired := ok && data.ExpiresAt.Before(time.Now())

	if !ok || expired {
		// If data is not cached OR
		// data is cached but has expired
		// schedule re-fetch 
		select {
		case d.fetchQueue <- w:
		default:
		}

		if (expired){
			// If expired return existing data to respond instantly

			// Set the expiry date to the future to not send further
			// requests for the same word until the fetch queue is processed.
			data.ExpiresAt = time.Now().Add(time.Minute)
			d.mut.Lock()
			d.data[w] = data
			d.mut.Unlock()
		}
	}

	if !ok {
		return nil, errQueued
	}

	out := make([]string, 0, len(data.Meanings))

	if data.Valid {
		if data.Found {
			for _, m := range data.Meanings {
				out = append(out, fmt.Sprintf("%s 1 TXT \"%s:\" \"%s\"", w, m.PartOfSpeech, m.Definition))
			}
		} else {
			out = append(out, fmt.Sprintf("%s 1 TXT \"word definition was not found in our dictionary, please try other sources.\"", w))
		}
	} else {
		out = append(out, fmt.Sprintf("%s 1 TXT \"dictionary unavailable, try again later.\"", w))
	}

	return out, nil
}

func (d *Dictionary) fetchAPI(w string) (entry, error){
	
	// If the request fails, still cache the bad result with a TTL to avoid
	// flooding the upstream with subsequent requests.
	bad := entry{Word: w, Valid: false, ExpiresAt: time.Now().Add(time.Minute * 10)}

	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf(apiUrl, w), nil)
	
	if err != nil {
		return bad, err
	}
	req.Header.Add("User-Agent", d.opt.UserAgent)

	res, err := d.client.Do(req)

	if err != nil {
		return bad, err
	}

	defer func() {
		// Drain and close the body to let the Transport reuse the connection
		io.Copy(ioutil.Discard, res.Body)
		res.Body.Close()
	}()


	defer res.Body.Close()

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return bad, err
	}

	if res.StatusCode == http.StatusInternalServerError || res.StatusCode == http.StatusTooManyRequests {
		return bad, errors.New("error fetching dictionaryapi")
	}
	var apiData []wordData
	var notFound wordNotFound

	// when word is not found, body is different
	err = json.Unmarshal(body, &notFound)

	out := entry{
		Word: w,
		Valid: true,
		Found: false,
		// not found cached for a month
		ExpiresAt: time.Now().AddDate(0, 1, 0),
	}

	// successfully unmarshalled with notFound body
	// send errNotFound
	if err == nil {
		return out, errNotFound
	}

	if err := json.Unmarshal(body, &apiData); err != nil {
		return bad, err
	}

	// word found
	out.Found = true
	// word found, cache for a long time as its definition is unlikely to change
	// so cached for a year
	out.ExpiresAt = time.Now().AddDate(1, 0, 0)

	// Keeping in mind dns TXT size limits
	// Only keep one Definition per PartOfSpeech from result 
	if len(apiData) > 0 {
		first := apiData[0]
		for _, p := range first.Meanings {
			if len(p.Definitions) == 0 {
				continue
			}
			definition := p.Definitions[0].Definition
			out.Meanings = append(out.Meanings, Meaning{
				PartOfSpeech: p.PartOfSpeech,
				Definition: strings.Trim(definition, " "),
			})
		}
	}
	return out, nil
}

// Dump produces a gob dump of the cached data.
func (d *Dictionary) Dump() ([]byte, error) {
	buf := &bytes.Buffer{}

	d.mut.RLock()
	defer d.mut.RUnlock()

	if err := gob.NewEncoder(buf).Encode(d.data); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// Load loads a gob dump of cached data.
func (d *Dictionary) Load(b []byte) error {
	buf := bytes.NewBuffer(b)

	d.mut.RLock()
	defer d.mut.RUnlock()

	err := gob.NewDecoder(buf).Decode(&d.data)
	return err
}