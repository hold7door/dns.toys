package dictionary

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"strings"
	"sync"
)

const apiUrl = "https://api.dictionaryapi.dev/api/v2/entries/en/%s"
const apiRateLimit = 10


// TODO: rate limit to 450 requests / 5 min
// TODO: look into TXT record size
// TODO: look into caching behaviors
// TODO: Implement save snapshots of data
// TODO: add Opt option to send user-agent

type Dictionary struct {
	data map[string]entry

	fetchQueue chan string

	mut sync.RWMutex

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


func New() *Dictionary {
	d := &Dictionary{
		data: make(map[string]entry),
		fetchQueue: make(chan string),

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
			res, err := d.fetchAPI(w)

			if err != nil && err != errNotFound {
				log.Printf("error fetching dictionaryapi API: %v", err)
				continue
			}

			// Cache correct results as well as
			// word not found errors
			d.mut.Lock()
			d.data[w] = res
			d.mut.Unlock()

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
			r := fmt.Sprintf("%s 1 TXT \"word meanings is being fetched. Try again in a few seconds.\"", q)
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

	if !ok {
		select {
		case d.fetchQueue <- w:
		default:
		}

		return nil, errQueued
	}

	out := make([]string, 0, len(data.Meanings))

	if data.Valid {
		for _, m := range data.Meanings {
			out = append(out, fmt.Sprintf("%s 1 TXT \"%s\" \"%s\"", w, m.PartOfSpeech, m.Definition))
		}
	} else {
		out = append(out, fmt.Sprintf("%s 1 TXT \"meaning of word was not found in our dictionary\"", w))
	}

	return out, nil
}

func (d *Dictionary) fetchAPI(w string) (entry, error){
	
	bad := entry{Word: w, Valid: false}

	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf(apiUrl, w), nil)
	
	if err != nil {
		return bad, err
	}
	req.Header.Add("User-Agent", "github.com/knadh/dns.toys")

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

	err = json.Unmarshal(body, &notFound)

	if err == nil {
		return bad, errNotFound
	}

	if err := json.Unmarshal(body, &apiData); err != nil {
		return bad, err
	}

	if len(apiData) > 0 {

	}

	out := entry{
		Word: w,
		Valid: true,
	}

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
func (c *Dictionary) Dump() ([]byte, error) {
	return nil, nil
}
