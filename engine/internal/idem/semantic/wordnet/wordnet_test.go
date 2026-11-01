package wordnet

import "testing"

func TestSynonyms(t *testing.T) {
	same := [][2]string{{"book", "reserve"}, {"buy", "purchase"}, {"rent", "hire"}, {"ship", "send"}, {"booked", "reserved"}, {"stolen", "steal"}}
	diff := [][2]string{{"buy", "sell"}, {"standard", "express"}, {"delete", "snapshot"}, {"suite", "deluxe"}, {"pune", "nagpur"}}
	for _, p := range same {
		if !Synonyms(p[0], p[1]) {
			t.Errorf("%s/%s should be synonyms", p[0], p[1])
		}
	}
	for _, p := range diff {
		if Synonyms(p[0], p[1]) {
			t.Errorf("%s/%s should not be synonyms", p[0], p[1])
		}
	}
}
