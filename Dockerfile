FROM scratch
COPY spec.yaml spec.yaml
COPY README.md README.md
COPY main.wasm plugin.wasm
