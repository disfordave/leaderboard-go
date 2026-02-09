# ---------- build stage ----------
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o server .

# ---------- runtime stage ----------
FROM alpine:3.20
WORKDIR /app
COPY --from=build /app/server /app/server
EXPOSE 8080
CMD ["/app/server"]
