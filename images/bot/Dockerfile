FROM golang:1.22-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/botd ./cmd/botd

FROM debian:bookworm-slim
ARG NODE_VERSION=22.23.4
ARG CLAUDE_CODE_VERSION=2.1.322
ARG CODEX_VERSION=0.167.1
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates wget curl xz-utils \
    && curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.xz" \
        | tar -xJ -C /usr/local --strip-components=1 \
    && npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" "@openai/codex@${CODEX_VERSION}" \
    && claude --version \
    && codex --version \
    && groupadd --gid 1000 pvebot \
    && useradd --uid 1000 --gid 1000 --create-home --shell /usr/sbin/nologin pvebot \
    && apt-get purge -y curl xz-utils \
    && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/* /root/.npm
COPY --from=build /out/botd /usr/local/bin/botd
WORKDIR /workspace
EXPOSE 8081
ENTRYPOINT ["botd"]
