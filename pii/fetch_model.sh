#!/bin/sh
# fetch the rampart ner bundle (model + vocab) into pii/model/.
# cc by 4.0, see NOTICE. needs: curl or python3 with huggingface_hub.
set -e
cd "$(dirname "$0")"
REV="b1993e4e68b082835b80ffc65acc03325ea2e501"
BASE="https://huggingface.co/nationaldesignstudio/rampart/resolve/$REV"
# pinned sha256 of the two blobs at $REV. an http error page must never
# become the model (silent NER degradation to heuristics).
SHA_MODEL="REPLACE_WITH_MODEL_SHA256"
SHA_VOCAB="REPLACE_WITH_VOCAB_SHA256"
mkdir -p model
if [ ! -f model/model_q4.onnx ]; then
  echo "fetching model_q4.onnx (14.7mb)..."
  curl -fSL --proto '=https' --tlsv1.2 -o model/model_q4.onnx "$BASE/onnx/model_q4.onnx"
fi
if [ ! -f model/vocab.txt ]; then
  echo "fetching vocab.txt..."
  curl -fSL --proto '=https' --tlsv1.2 -o model/vocab.txt "$BASE/vocab.txt"
fi
if [ "$SHA_MODEL" != "REPLACE_WITH_MODEL_SHA256" ]; then
  echo "$SHA_MODEL  model/model_q4.onnx" | sha256sum -c -
fi
if [ "$SHA_VOCAB" != "REPLACE_WITH_VOCAB_SHA256" ]; then
  echo "$SHA_VOCAB  model/vocab.txt" | sha256sum -c -
fi
ls -la model/
