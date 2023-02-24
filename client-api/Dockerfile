FROM golang:1.19-alpine

WORKDIR /app

COPY go.mod ./
RUN go mod download

COPY . ./
RUN go build -o /clientapi

EXPOSE 3001

CMD [ "/clientapi" ]
