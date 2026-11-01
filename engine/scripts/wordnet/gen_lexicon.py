"""Generate the compact WordNet synonym lexicon embedded by
internal/idem/semantic/wordnet.

Output (gzip, UTF-8, one record per line):
  S<TAB>word<TAB>id,id,...   every synset (any part of speech) the lemma is in
  X<TAB>form<TAB>base        WordNet's irregular-form exception lists

Only single-word, purely alphabetic lemmas are kept. Synset ids are dense
integers assigned in WordNet's own order, so the file is deterministic.
WordNet 3.0, Princeton University; see LICENSE-WORDNET in the same directory.
"""
import gzip, sys, warnings
warnings.filterwarnings("ignore")
from nltk.corpus import wordnet as wn

out = sys.argv[1]
ids = {}
rows = {}
for syn in wn.all_synsets():
    sid = ids.setdefault(syn.name(), len(ids))
    for lem in syn.lemma_names():
        w = lem.lower()
        if not w.isalpha():
            continue
        rows.setdefault(w, []).append(sid)
exc = []
for pos in "nvar":
    for form, bases in wn._exception_map[pos].items():
        if form.isalpha() and bases and bases[0].isalpha():
            exc.append((form, bases[0]))
with gzip.open(out, "wt", encoding="utf-8", compresslevel=9) as f:
    for w in sorted(rows):
        f.write("S\t%s\t%s\n" % (w, ",".join(str(i) for i in sorted(set(rows[w])))))
    for form, base in sorted(set(exc)):
        f.write("X\t%s\t%s\n" % (form, base))
print(len(rows), "lemmas,", len(ids), "synsets,", len(set(exc)), "exceptions")
