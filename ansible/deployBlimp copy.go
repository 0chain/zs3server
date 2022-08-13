package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os/exec"

	"github.com/gorilla/mux"
)

type BlimpUser struct {
    IP                      string `json:"IP"`
    Server_UserName         string `json:"Server_UserName"`
    ConfigurationDirectory  string `json:"ConfigurationDirectory"`
    MinioUserName           string `json:"MinioUserName"`
	MinioPassword           string `json:"MinioPassword"`
    AllocationId            string `json:"AllocationId"`
    Port                    string `json:"Port"`
    ConsoleBlimp            string `json:"ConsoleBlimp"`
    Url                     string `json:"Url"`
}

func DeployBlimp(w http.ResponseWriter, r *http.Request) {

	var newUser BlimpUser
    reqBody, err := ioutil.ReadAll(r.Body)
    if err != nil {
        fmt.Fprintf(w, "Kindly enter data with the event title and description only in order to update")
    }
    json.Unmarshal(reqBody, &newUser)
    fmt.Println("got params")

    cmd := exec.Command("sh", "./minio_script.sh", newUser.IP, newUser.Server_UserName, 
    newUser.ConfigurationDirectory, newUser.MinioUserName, newUser.MinioPassword, 
    newUser.AllocationId, newUser.ConsoleBlimp, newUser.Port)
        
    // bash minio_script.sh 3.144.74.110 root $HOME/.zcn manali manalipassword 773dde936212cb60b312b1577a7a21aae4a4114b7ece242b8c2be5851b3656c4
    // bash minio_script.sh IP server_Username configDirectory miniousername miniopassword allocationID port console
    
    out, err := cmd.CombinedOutput()
    if err != nil {
        log.Fatal(err.Error())
    }
    newUser.Url = "http://" + newUser.IP + ":9000"
    w.WriteHeader(http.StatusCreated)
    json.NewEncoder(w).Encode(newUser)
    fmt.Println(string(out))
}

func main() {

    router := mux.NewRouter().StrictSlash(true)
    router.HandleFunc("/deploy-blimp", DeployBlimp).Methods("POST")
    fmt.Println("server started")
    log.Fatal(http.ListenAndServe(":8080", router))

}