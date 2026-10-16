# naira-sim and simctl in one small image.
ARG GO_VERSION=1.27.2

FROM golang:${GO_VERSION} AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/nairasim ./cmd/nairasim \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/simctl ./cmd/simctl

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
COPY examples/scenarios /scenarios
EXPOSE 7070
ENTRYPOINT ["nairasim"]
