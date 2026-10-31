FROM golang:1.27.3-alpine AS build

ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/nerdneilsfield/go-template/internal/buildinfo.Version=${VERSION}" \
    -o /out/go-template ./

FROM gcr.io/distroless/static-debian13:nonroot

USER nonroot:nonroot
WORKDIR /app
COPY --from=build /out/go-template /app/go-template

ENTRYPOINT ["/app/go-template"]
