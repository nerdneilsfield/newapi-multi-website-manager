FROM golang:1.27.3-alpine AS build

ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/nerdneilsfield/newapi-multi-website-manager/internal/buildinfo.Version=${VERSION}" \
    -o /out/napm ./

FROM gcr.io/distroless/static-debian13:nonroot

USER nonroot:nonroot
WORKDIR /data
COPY --from=build /out/napm /app/napm

EXPOSE 8080
VOLUME /data
ENTRYPOINT ["/app/napm"]
CMD ["serve", "--addr", "0.0.0.0:8080", "--db", "/data/napm.db"]
