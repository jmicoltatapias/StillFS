FROM node:24-alpine AS frontend
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.27-alpine AS backend
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/stillfs ./cmd/stillfs
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/stillfsctl ./cmd/stillfsctl

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && addgroup -S stillfs && adduser -S -G stillfs stillfs && mkdir -p /app/data/objects && chown -R stillfs:stillfs /app/data
WORKDIR /app
COPY --from=backend /out/stillfs /app/stillfs
COPY --from=backend /out/stillfsctl /app/stillfsctl
COPY --from=frontend /src/web/dist /app/web/dist
USER stillfs
EXPOSE 9000
ENTRYPOINT ["/app/stillfs"]
