FROM golang:1.21-alpine

RUN apk update && apk add --no-cache git

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . ./

RUN go build -o /zsearch

# VOLUME ["/vindex"]

EXPOSE 3003

CMD ["/zsearch"]