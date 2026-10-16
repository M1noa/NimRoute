#!/bin/sh
# fetch the rampart ner bundle (model + vocab) into pii/model/.
# cc by 4.0, see NOTICE. needs: curl or python3 with huggingface_hub.
set -e
cd "$(dirname "$0")"
if ! command -v curl >/dev/null 2>&1; then
  echo "fetch_model.sh: curl is required to download the Rampart NER model" >&2
  echo "                install curl (macOS: xcode-select --install; Linux: apt install curl)" >&2
  exit 1
fi
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
  echo "fetch_model.sh: sha256sum or shasum is required to verify model checksums" >&2
  exit 1
fi
REV="b1993e4e68b082835b80ffc65acc03325ea2e501"
BASE="https://huggingface.co/nationaldesignstudio/rampart/resolve/$REV"
# pinned sha256 of the two blobs at $REV. an http error page must never
# become the model (silent NER degradation to heuristics).
SHA_MODEL="9f27d24949b0581701071ea5ef522d77ccd3f50c525cc91eac4d265b0fc2afe5"
SHA_VOCAB="0fbe6b50061feabb9be68af471e9aa6df07a4bc428bdca4b0eff1fcd3612dee5"
mkdir -p model
if [ ! -f model/model_q4.onnx ]; then
  echo "fetching model_q4.onnx (14.7mb)..."
  curl -fSL --proto '=https' --tlsv1.2 -o model/model_q4.onnx "$BASE/onnx/model_q4.onnx"
fi
if [ ! -f model/vocab.txt ]; then
  echo "fetching vocab.txt..."
  curl -fSL --proto '=https' --tlsv1.2 -o model/vocab.txt "$BASE/vocab.txt"
fi
# gnu sha256sum, or the macos equivalent (coreutils is not a given).
if command -v sha256sum >/dev/null 2>&1; then
  SHA="sha256sum"
else
  SHA="shasum -a 256"
fi
echo "$SHA_MODEL  model/model_q4.onnx" | $SHA -c -
echo "$SHA_VOCAB  model/vocab.txt" | $SHA -c -
ls -la model/
