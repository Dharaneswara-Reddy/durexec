"""Generate the place-alias table embedded by internal/idem/semantic/places.

Source: the airportsdata package (MIT licence), every airport with an IATA
code. Each line: CODE<TAB>alias alias ... where the aliases are the city name
and the proper-noun words of the airport name that are not ordinary English
words (so "Bengaluru" from "Bengaluru International Airport" is kept and
"International" is not). All lower case.
"""
import gzip, sys, warnings, re
warnings.filterwarnings("ignore")
import airportsdata
from nltk.corpus import wordnet as wn

GENERIC = {"airport", "international", "intl", "regional", "municipal", "airfield", "air", "base", "field",
           "national", "domestic", "county", "city", "new", "old", "north", "south", "east", "west", "saint", "st"}
out = sys.argv[1]
rows = []
for code, a in sorted(airportsdata.load("IATA").items()):
    if not re.fullmatch(r"[A-Z]{3}", code):
        continue
    words = set()
    city = a["city"].lower().strip()
    for w in re.findall(r"[a-z]+", city):
        if w not in GENERIC and len(w) > 2:
            words.add(w)
    for w in re.findall(r"[A-Za-z]+", a["name"]):
        lw = w.lower()
        if lw in GENERIC or len(lw) <= 2 or not w[0].isupper():
            continue
        if wn.synsets(lw):
            continue  # an ordinary English word, not a name
        words.add(lw)
    if words:
        rows.append(code.lower() + "\t" + " ".join(sorted(words)))
with gzip.open(out, "wt", encoding="utf-8", compresslevel=9) as f:
    f.write("\n".join(rows) + "\n")
print(len(rows), "codes")
