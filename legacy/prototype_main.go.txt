package main

import (
	"io/ioutil"
	"log"
	"os"

	"github.com/aws/aws-sdk-go/aws/credentials"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/lxc/lxd/client"
	"github.com/lxc/lxd/shared/api"
)

//test comment
func checkFileForError(e error) {
	if e != nil {
		panic(e)
	}
}

func exitErrorf(msg string, args ...interface{}) {
	//fmt.Fprintf(os.Stderr, msg+"\n", args...)
	log.Print(msg+"\n", args)
	os.Exit(1)
}

//GetFileFromS3 is a reusable function. Just call it and tell it which files to download.
func GetFileFromS3(S3itemToDOwnload string) {

	log.Print("Started Downloading from s3 Function...")

	KeyID := os.Getenv("KeyID")
	SecretKey := os.Getenv("SecretKey")
	TokenForSession := os.Getenv("AWS_SESSION_TOKEN")
	bucket := "lxd-server-certificates"
	item := S3itemToDOwnload

	/*os.Setenv("AWS_ACCESS_KEY_ID", KeyID)
	os.Setenv("AWS_SECRET_ACCESS_KEY", SecretKey)*/

	file, err := os.Create("/tmp/" + item)
	if err != nil {
		exitErrorf("Unable to open file %q, %v", err)
		log.Print("Could not even create file in the file system...")
	} else {
		log.Print("Created file in the file system...")
	}

	defer file.Close()

	// Initialize a session in us-east-1.
	sess, err := session.NewSession(&aws.Config{
		Region:      aws.String("us-east-1"),
		Credentials: credentials.NewStaticCredentials(KeyID, SecretKey, "")},
	)
	if err != nil {
		log.Print("Could not start download session for S3 because of: ", err)
	}

	log.Print("KeyID is ", KeyID)
	log.Print("SecretKey is ", SecretKey)
	log.Print("TokenForSession is ", TokenForSession)

	downloader := s3manager.NewDownloader(sess)

	numBytes, err := downloader.Download(file,
		&s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(item),
		})
	if err != nil {
		exitErrorf("Unable to download item %q, %v", item, err)
		log.Print("Could not downloads certs from S3 because of: ", err)
	}

	log.Print("Downloaded", file.Name(), numBytes, "bytes")
}

func connectToLXDserver() error {

	log.Print("started main function...")

	cloudComputerName := "madewithapiLambda-1"

	//declare certs for passing to AWS S3 function
	serverCertFromS3 := "ec2-lxd-server-for-go-api.crt"
	clientCertFromS3 := "lxd-type3access.crt"
	clientKeyFromS3 := "lxd-type3access.key"

	GetFileFromS3(serverCertFromS3)
	GetFileFromS3(clientCertFromS3)
	GetFileFromS3(clientKeyFromS3)

	// Connection parameters - LXD API needs to know client cert, key and server cert
	ClientCertFile, errcert := ioutil.ReadFile("/tmp/" + clientCertFromS3)
	checkFileForError(errcert)
	ClientCertString := string(ClientCertFile)

	ClientKeyFile, errkey := ioutil.ReadFile("/tmp/" + clientKeyFromS3)
	checkFileForError(errkey)
	ClientKeyString := string(ClientKeyFile)

	ServerCertFile, errservercert := ioutil.ReadFile("/tmp/" + serverCertFromS3)
	checkFileForError(errservercert)
	ServerCertString := string(ServerCertFile)

	argumentsToPass := &lxd.ConnectionArgs{
		TLSClientCert: ClientCertString,
		TLSClientKey:  ClientKeyString,
		TLSServerCert: ServerCertString,
		/*InsecureSkipVerify: true*/}

	// Connect to LXD over http
	c, err := lxd.ConnectLXD("https://172.30.2.171:8443", argumentsToPass)
	if err != nil {
		log.Print("Could not connect because of error: ", err)
		///log.Print("Could not connect because of some error...")
		return err
	}

	// Container creation request
	req := api.ContainersPost{
		Name: cloudComputerName,
		Source: api.ContainerSource{
			Type:  "image",
			Alias: "image4go",
		},
	}

	// Get LXD to create the container (background operation)
	op, err := c.CreateContainer(req)
	if err != nil {
		log.Print("Could not create container because of error: ", err)
		return err
	}

	// Wait for the operation to complete
	err = op.Wait()
	if err != nil {
		log.Print("Could not wait for operation because of error: ", err)
		return err
	}

	// Get LXD to start the container (background operation)
	reqState := api.ContainerStatePut{
		Action:  "start",
		Timeout: -1,
	}

	op, err = c.UpdateContainerState(cloudComputerName, reqState, "")
	if err != nil {
		log.Print("Could not update container status because of error: ", err)
		return err
	}

	// Wait for the operation to complete
	err = op.Wait()
	if err != nil {
		log.Print("Could not wait because of error: ", err)
		return err
	}
	return err
}

func main() {
	connectToLXDserver()
}
