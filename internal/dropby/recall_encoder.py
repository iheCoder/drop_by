"""Local BGE-M3 encoder. stdin/stdout carry one JSON request/response per line."""
import hashlib
import json
import os
import sqlite3
import sys

os.umask(0o077)
os.environ.update(HF_HUB_OFFLINE="1", TRANSFORMERS_OFFLINE="1", TOKENIZERS_PARALLELISM="false")
import numpy as np
import torch
from transformers import AutoModel, AutoTokenizer

# Pin the encoder and pooling convention: persisted vectors must share the query's space.
revision = "5617a9f61b028005a4858fdac845db406aefb181"
options = dict(revision=revision, local_files_only=True)
tokenizer = AutoTokenizer.from_pretrained("BAAI/bge-m3", **options)
model = AutoModel.from_pretrained("BAAI/bge-m3", **options)
device = "mps" if torch.backends.mps.is_available() else "cpu"
if device == "mps":
    model.half()
model.to(device).eval()
cache = sqlite3.connect(sys.argv[1])
cache.execute("CREATE TABLE IF NOT EXISTS vectors (key BLOB PRIMARY KEY, vector BLOB NOT NULL)")


def encode(texts):
    with torch.inference_mode():
        tokens = tokenizer(texts, padding=True, return_tensors="pt").to(device)
        vectors = model(**tokens).last_hidden_state[:, 0, :].float()
        return torch.nn.functional.normalize(vectors, dim=1).cpu().numpy()


def passages(body):
    # Keep natural lines together. Long individual lines (including HTML) are still searchable.
    block = ""
    for line in body.splitlines():
        line = line.strip()
        while line:
            room = 200 - len(block)
            if room <= 1 or (block and len(line) + 1 > room):
                yield block
                block, room = "", 200
            block = (block + " " + line[:room]).strip()
            line = line[room:]
    if block:
        yield block


def search(request):
    # The caller supplies only records visible *now*. Cache hits can never add candidates.
    # Cache content hashes and vectors only. Editing a passage changes the lookup key.
    texts, owners = [], []
    for index, (title, body) in enumerate(request["documents"]):
        # An unusually long title is itself searchable in full, without overflowing
        # the encoder's context window on every body passage.
        if len(title) > 200:
            for part in passages(title):
                texts.append(part)
                owners.append(index)
        for snippet in passages(body):
            texts.append((title[:200] + " " + snippet).strip())
            owners.append(index)
        if not owners or owners[-1] != index:
            texts.append(title)
            owners.append(index)

    vectors, missing = {}, {}
    for text in dict.fromkeys(texts):
        key = hashlib.sha256((revision + "/cls/" + device + "/" + text).encode()).digest()
        row = cache.execute("SELECT vector FROM vectors WHERE key=?", (key,)).fetchone()
        if row:
            vectors[text] = np.frombuffer(row[0], np.float32)
        else:
            missing[text] = key
    pending = list(missing)
    for start in range(0, len(pending), 12):
        batch = pending[start:start + 12]
        for text, vector in zip(batch, encode(batch)):
            vectors[text] = vector
            cache.execute("INSERT OR REPLACE INTO vectors VALUES (?,?)", (missing[text], vector.tobytes()))
        cache.commit()

    scores = np.stack([vectors[text] for text in texts]) @ encode([request["query"]])[0]
    best = {}
    for i, owner in enumerate(owners):
        if owner not in best or scores[i] > scores[best[owner]]:
            best[owner] = i
    # Keep the actual cosine gap: two adjacent ranks can be nearly tied or far apart.
    return [dict(index=owner, match=texts[i], score=float(scores[i]))
            for owner, i in sorted(best.items(), key=lambda item: -scores[item[1]])]


for line in sys.stdin:
    try:
        result = {"hits": search(json.loads(line))}
    except Exception as error:
        cache.rollback()
        result = {"error": str(error)}
    print(json.dumps(result, ensure_ascii=False), flush=True)
