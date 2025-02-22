FROM --platform=$BUILDPLATFORM golang:1.23 AS build

COPY go.mod go.sum /app/
WORKDIR /app
RUN go mod download

COPY . /app
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -o /app/mcas

FROM gcr.io/distroless/static-debian12
COPY --from=build /app/mcas /mcas
ENTRYPOINT ["/mcas"]
