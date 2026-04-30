FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/shipd ./cmd/shipd

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/shipd /shipd
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/shipd"]
CMD ["serve", "--addr", ":8080", "--data-dir", "/data"]
