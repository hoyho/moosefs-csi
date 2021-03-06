/*
   Copyright 2018 Tuxera Oy. All Rights Reserved.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.

*/

package driver

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecs"
	csi "github.com/container-storage-interface/spec/lib/go/csi/v0"
)

var (
	mfsTypeMaster = MfsType{name: "moosefs-master", version: "0.0.2"}
)

type AwsCreds struct {
	ID     string
	secret string
	token  string
}

type MfsType struct {
	name    string
	version string
	Env     []*ecs.KeyValuePair // TODO(anoop): remove
}

// Maintains the AWS entities created during ECS creation
type ECSStore struct {
	Service       *ecs.CreateServiceOutput
	Task          *ecs.RegisterTaskDefinitionOutput
	SecurityGroup *ec2.SecurityGroup
	TaskList      *ecs.ListTasksOutput
}

type AWSCreateVolOutput struct {
	volID  string
	Ec2Res *ec2.Reservation
}

// TODO(anoop): AWS/GCP/Azure credentials
// TODO(anoop): Check for storage distribution (master, chunk etc.)
func AWSCreateVol(volName string, d *Driver, volSizeInGB int64) (AWSCreateVolOutput, error) {

	output := AWSCreateVolOutput{}

	// Create AWS Session
	sess, err := CreateAWSSession(d)

	ll := d.log.WithFields(logrus.Fields{
		"volumeName": volName,
		"method":     "AWSCreateVol",
		"region":     d.awsRegion,
	})

	// Create the fargate cluster for master
	_, err = CreateECSCluster(sess, volName)
	if err != nil {
		return output, err
	}
	ll.Info("create ECSCluster completed")

	// Create the fargate master service
	store, err := CreateECSService(sess, d, volName, volName, mfsTypeMaster)
	if err != nil {
		return output, err
	}
	ll.Info("create master ECSService completed")

	// Get Master endpoint
	ep, err := GetPublicIP4(sess, d.awsRegion, volName, *store.TaskList.TaskArns[0])
	if err != nil {
		return output, err
	}
	ll.Info("Obtained publicIP4")

	volID := *ep + ":"
	// Attach chunkserver volumes
	createEc2Output, err := CreateEc2Instance(d, *store.SecurityGroup.GroupName, *ep, volID, volSizeInGB, sess)
	if err != nil {
		return output, err
	}
	ll.Info("create EC2 chunk servers completed")
	output.volID = volID
	output.Ec2Res = createEc2Output

	return output, nil // 35.228.134.224:

}

// AWSDeleteVol ...
func AWSDeleteVol(volID string, d *Driver) error {

	// Create AWS Session
	sess, err := CreateAWSSession(d)

	_, err = DeleteEc2Instance(volID, d, sess)
	if err != nil {
		return err
	}

	volName := volID // TODO(anoop): get volName from volID
	_, err = DeleteECSService(sess, d.awsRegion, volName, volName, ECSStore{})
	if err != nil {
		return err
	}
	return nil
}

// CreateAWSSession ...
func CreateAWSSession(d *Driver) (*session.Session, error) {
	sess, err := session.NewSession(
		&aws.Config{Region: aws.String(d.awsRegion),
			Credentials: credentials.NewStaticCredentials(d.awsAccessKey, d.awsSecret, d.awsSessionToken),
		})

	if err != nil {
		return nil, err
	}
	return sess, nil

}

// CreateECSCluster ...
func CreateECSCluster(sess *session.Session, name string) (*ecs.CreateClusterOutput, error) {

	svc := ecs.New(sess)
	input := &ecs.CreateClusterInput{
		ClusterName: aws.String(name),
	}

	result, err := svc.CreateCluster(input)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// DeleteECSCluster ...
func DeleteECSCluster(creds AwsCreds, region, name string) (*ecs.DeleteClusterOutput, error) {
	sess, err := session.NewSession(
		&aws.Config{Region: aws.String(region),
			Credentials: credentials.NewStaticCredentials(creds.ID, creds.secret, creds.token),
		})
	if err != nil {
		return nil, err
	}

	svc := ecs.New(sess)
	input := &ecs.DeleteClusterInput{
		Cluster: aws.String(name),
	}

	result, err := svc.DeleteCluster(input)
	if err != nil {
		return nil, err
	}

	return result, nil
}

// CreateECSService ...
func CreateECSService(sess *session.Session, d *Driver, name, clusterName string, mfsType MfsType) (ECSStore, error) {
	store := ECSStore{}
	ll := d.log.WithFields(logrus.Fields{
		"method":         "CreateECSService",
		"serviceName":    name,
		"clusterName":    clusterName,
		"taskDefinition": mfsType.name,
	})

	// Register task definition
	svc := ecs.New(sess)
	output, err := registerTaskDefinition(svc, mfsType)
	if err != nil {
		return store, err
	}
	store.Task = output
	ll.Info("Task definition registered")

	// Create securityGroup
	sgs, err := createSecurityGroup(clusterName, "Created for moosefs-csi-fargate", d.awsRegion, sess)
	if err != nil {
		return store, err
	}
	store.SecurityGroup = sgs[0]
	ll.Info("SecurityGroup set")

	//Check and Create the service
	svcInput := &ecs.DescribeServicesInput{
		Cluster:  aws.String(clusterName),
		Services: []*string{aws.String(name)},
	}
	svcOutput, err := svc.DescribeServices(svcInput)
	if err != nil {
		return store, err
	}
	ll.Info("DescribeServices completed")
	// Check if Service exists
	if len(svcOutput.Services) == 0 || (svcOutput.Services[0] != nil && *svcOutput.Services[0].Status != "ACTIVE") {

		gid := *sgs[0].GroupId
		input := &ecs.CreateServiceInput{
			Cluster:        aws.String(clusterName),
			DesiredCount:   aws.Int64(1),
			ServiceName:    aws.String(name),
			TaskDefinition: aws.String(mfsType.name),
			LaunchType:     aws.String("FARGATE"),
			NetworkConfiguration: &ecs.NetworkConfiguration{
				AwsvpcConfiguration: &ecs.AwsVpcConfiguration{
					AssignPublicIp: aws.String(ecs.AssignPublicIpEnabled),
					SecurityGroups: aws.StringSlice([]string{gid}),
					Subnets:        aws.StringSlice(createSubnets(clusterName)),
				},
			},
		}
		result, err := svc.CreateService(input)
		if err != nil {
			return store, err
		}
		store.Service = result
		ll.Info("Service created")
	}

	// Wait for task running // TODO(anoop): This is not enough, sometimes 'Association empty for NetworkInterface:'
	if err := waitUntilTaskArn(clusterName, svc, 60); err != nil {
		return store, err
	}
	ll.Info("TaskArns obtained")

	// List tasks for Arns
	listTaskInput := &ecs.ListTasksInput{
		Cluster: aws.String(clusterName),
	}
	listTaskOutput, err := svc.ListTasks(listTaskInput)
	store.TaskList = listTaskOutput
	ll.Info("TaskList obtained")

	if err = waitUntilTaskActive(clusterName, *listTaskOutput.TaskArns[0], svc, 60); err != nil {
		return store, err
	}
	ll.Info("Task running")

	return store, nil
}

// DeleteECSService ...
// TODO(anoop): Handle SG
func DeleteECSService(sess *session.Session, region, name, clusterName string, store ECSStore) (*ecs.DeleteServiceOutput, error) {
	// svc := ecs.New(sess)

	// TODO:(anoop) Stop services before deleting

	// TODO(anoop): Figure-out a way to derive store to deRegisterTaskDefinition()

	// De-Register task definition
	// taskRev := strconv.FormatInt(*store.Task.TaskDefinition.Revision, 10)
	// if _, err := deregisterTaskDefinition(svc, *store.Task.TaskDefinition.Family, taskRev); err != nil {
	// 	return nil, err
	// }

	// Delete the service
	/*
		input := &ecs.DeleteServiceInput{
			Cluster: aws.String(clusterName),
			Service: aws.String(name),
			Force:   aws.Bool(true),
		}
		result, err := svc.DeleteService(input)
		if err != nil {
			return nil, err
		}
	*/
	// Delete security group
	/* TODO(anoop): Needs waiting for Ec2 instance shutdown
	if err := deleteSecurityGroup(*store.SecurityGroup.GroupId, region); err != nil {
		return nil, err
	}
	*/
	return &ecs.DeleteServiceOutput{}, nil
}

// CreateEc2Instance ...
// TODO(anoop): not idempotent
// TODO(anoop): Wait for the chunkService
func CreateEc2Instance(d *Driver, sg, masterIP, volID string, volSizeInGB int64, sess *session.Session) (*ec2.Reservation, error) {
	devName := "/dev/xvdh"
	imageName := "amzn-ami-hvm-2018.03.0.20180412-x86_64-ebs" // ensure its in all regions
	ll := d.log.WithFields(logrus.Fields{
		"method":   "CreateEc2Instance",
		"endpoint": masterIP,
		"volID":    volID,
	})

	// Create an EC2 service client.
	svc := ec2.New(sess)

	// Check if already instance exists
	descInstanceInput := &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{
			{
				Name: aws.String("tag:volID"),
				Values: []*string{
					aws.String(volID),
				},
			},
		},
	}
	descInstanceOutput, err := svc.DescribeInstances(descInstanceInput)
	if len(descInstanceOutput.Reservations) > 0 {
		ll.Info("Instance already exists, skipping creation")
		return descInstanceOutput.Reservations[0], nil
	}

	// Obtain the imageID
	ll.Info("finding optimal Ec2 images")
	descInput := &ec2.DescribeImagesInput{
		Filters: []*ec2.Filter{
			&ec2.Filter{
				Name: aws.String("name"),
				Values: []*string{
					aws.String(imageName),
				},
			},
		},
	}
	descOutput, err := svc.DescribeImages(descInput)
	if err != nil {
		return nil, err
	}
	if len(descOutput.Images) < 1 || descOutput.Images[0].ImageId == nil {
		return nil, errors.New("Unable to fetch ImageID for ImageName: " + imageName)
	}
	imageID := descOutput.Images[0].ImageId
	userDataEncoded := encodedUserData(volSizeInGB, devName, masterIP)

	riInput := &ec2.RunInstancesInput{
		// KeyName:          aws.String("anoop_ireland"), // TODO(anoop): To be removed
		ImageId:          imageID,
		InstanceType:     aws.String(ec2.InstanceTypeT2Micro),
		MinCount:         aws.Int64(1),
		MaxCount:         aws.Int64(1),
		UserData:         aws.String(userDataEncoded),
		SecurityGroupIds: []*string{aws.String(sg)},
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("instance"),
				Tags: []*ec2.Tag{
					{
						Key:   aws.String("volID"),
						Value: aws.String(volID),
					},
				},
			},
		},
		BlockDeviceMappings: []*ec2.BlockDeviceMapping{
			{
				DeviceName: aws.String("/dev/xvdh"),
				Ebs: &ec2.EbsBlockDevice{
					VolumeSize: aws.Int64(volSizeInGB),
					//					DeleteOnTermination: aws.Bool(true),
				},
			},
		},
	}
	ll.Info("Creating Ec2instance")
	riOutput, err := svc.RunInstances(riInput)
	if err != nil {
		return nil, err
	}
	ll.Info("Ec2instance created")

	return riOutput, nil

}

// DeleteEc2Instance ...
func DeleteEc2Instance(volID string, d *Driver, sess *session.Session) (*ec2.TerminateInstancesOutput, error) {

	// Create an EC2 service client.
	svc := ec2.New(sess)

	ll := d.log.WithFields(logrus.Fields{
		"method": "delete_ec2_instance",
		"volID":  volID,
	})
	ll.Info("delete ec2 instance called")

	descInput := &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{
			{
				Name: aws.String("tag:volID"),
				Values: []*string{
					aws.String(volID),
				},
			},
		},
	}
	descOutput, err := svc.DescribeInstances(descInput)
	if err != nil {
		return nil, err
	}
	ll.Info("describe ec2 instance successful")

	var terminateOutput *ec2.TerminateInstancesOutput
	if len(descOutput.Reservations) > 0 && len(descOutput.Reservations[0].Instances) > 0 {
		terminateInput := &ec2.TerminateInstancesInput{
			InstanceIds: []*string{
				descOutput.Reservations[0].Instances[0].InstanceId,
			},
		}
		terminateOutput, err = svc.TerminateInstances(terminateInput)
		if err != nil {
			return nil, err
		}
	} else {
		return nil, errors.New("DescribeInstances didnt return Reservations or Instances")
	}
	ll.Info("Deleting ec2 instance successful")
	return terminateOutput, nil
}

func registerTaskDefinition(svc *ecs.ECS, mfsType MfsType) (*ecs.RegisterTaskDefinitionOutput, error) {
	image := "quay.io/tuxera/" + mfsType.name + ":" + mfsType.version
	input := &ecs.RegisterTaskDefinitionInput{
		Family:                  aws.String(mfsType.name), // Task Name
		Cpu:                     aws.String("256"),        // 0.25vCPU
		Memory:                  aws.String("512"),        // 512MB
		NetworkMode:             aws.String("awsvpc"),
		RequiresCompatibilities: aws.StringSlice([]string{"FARGATE"}),
		ContainerDefinitions: []*ecs.ContainerDefinition{
			{
				Essential: aws.Bool(true),
				Image:     aws.String(image),
				Name:      aws.String("moosefs-server"),
			},
		},
	}
	result, err := svc.RegisterTaskDefinition(input)
	if err != nil {
		return nil, err
	}

	return result, nil
}

func deregisterTaskDefinition(svc *ecs.ECS, taskName, revision string) (*ecs.DeregisterTaskDefinitionOutput, error) {
	input := &ecs.DeregisterTaskDefinitionInput{
		TaskDefinition: aws.String(taskName + ":" + revision),
	}
	result, err := svc.DeregisterTaskDefinition(input)
	if err != nil {
		return nil, err
	}

	return result, nil
}

func createSecurityGroup(name, desc, region string, sess *session.Session) ([]*ec2.SecurityGroup, error) {
	// Create an EC2 service client.
	svc := ec2.New(sess)

	// get VPC ID
	// Get a list of VPCs so we can associate the group with the first VPC.
	result, err := svc.DescribeVpcs(nil)
	if err != nil {
		return nil, err
	}
	if len(result.Vpcs) == 0 {
		return nil, errors.New("No VPCs found to associate security group with")
	}
	vpcID := aws.StringValue(result.Vpcs[0].VpcId)

	// check if already exists
	input := &ec2.DescribeSecurityGroupsInput{
		GroupNames: []*string{
			aws.String(name),
		},
	}
	grps, err := svc.DescribeSecurityGroups(input)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case "InvalidGroup.NotFound":
				// Create security group
				_, err := svc.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
					GroupName:   aws.String(name),
					Description: aws.String(desc),
					VpcId:       aws.String(vpcID),
				})
				if err != nil {
					return nil, err
				}
				_, err = svc.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
					GroupName: aws.String(name),
					IpPermissions: []*ec2.IpPermission{
						(&ec2.IpPermission{}).
							SetIpProtocol("tcp").
							SetFromPort(9419).
							SetToPort(9421).
							SetIpRanges([]*ec2.IpRange{
								{CidrIp: aws.String("0.0.0.0/0")},
							}),
						(&ec2.IpPermission{}).
							SetIpProtocol("tcp").
							SetFromPort(22).
							SetToPort(22).
							SetIpRanges([]*ec2.IpRange{
								(&ec2.IpRange{}).
									SetCidrIp("0.0.0.0/0"),
							}),
					},
				})
				if err != nil {
					return nil, err
				}
				// check if it exists now and return/fail
				grps, err = svc.DescribeSecurityGroups(input)
				if err != nil {
					return nil, err
				}
			default:
				return nil, err
			}
		} else {
			// Print the error, cast err to awserr.Error to get the Code and
			// Message from an error.
			fmt.Println(err.Error())
		}
	}

	return grps.SecurityGroups, nil
}

func deleteSecurityGroup(groupID, region string) error {
	// create svc
	sess, err := session.NewSession(&aws.Config{
		Region: aws.String(region)},
	)
	if err != nil {
		return err
	}
	// Create an EC2 service client.
	svc := ec2.New(sess)

	_, err = svc.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{
		GroupId: aws.String(groupID),
	})
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case "InvalidGroup.NotFound":
				break // IGNORE
			default:
				return err
			}
		}
	}
	return nil
}

func createSubnets(name string) []string {
	// TODO(anoop): Dynamic
	subnets := []string{"subnet-a47092ed"}
	return subnets
}

// GetPublicIP4 ...
func GetPublicIP4(sess *session.Session, region, clusterName, taskArn string) (*string, error) {

	svc := ecs.New(sess)

	taskInput := &ecs.DescribeTasksInput{
		Tasks:   []*string{aws.String(taskArn)},
		Cluster: aws.String(clusterName),
	}

	descTasks, err := svc.DescribeTasks(taskInput)
	if err != nil {
		return nil, err
	}

	// Tasks: [{
	//	Attachments: [{
	//		Details: [{
	//			Name: "networkInterfaceId",
	//			Value: "eni-94d894a3"
	//		},
	ni, err := extractNI(descTasks.Tasks[0].Attachments) // extract networkInterfaceId
	if err != nil {
		return nil, err
	}

	svcEc2 := ec2.New(sess)
	input := &ec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: []*string{
			aws.String(ni),
		},
	}
	descNIs, err := svcEc2.DescribeNetworkInterfaces(input)
	if err != nil {
		return nil, err
	}

	// NetworkInterfaces: [{
	//		Association: {
	//		IpOwnerId: "amazon",
	//		PublicDnsName: "ec2-52-48-31-230.eu-west-1.compute.amazonaws.com",
	//		PublicIp: "52.48.31.230"
	// },
	nis := descNIs.NetworkInterfaces
	if len(nis) < 1 {
		return nil, errors.New("Unable to obtain DescribeNetworkInterfaces for: " + ni)
	}
	if nis[0].Association == nil {
		return nil, errors.New("Association empty for NetworkInterface: " + ni)
	}
	return nis[0].Association.PublicIp, nil
}

func extractNI(attchmts []*ecs.Attachment) (string, error) {
	if len(attchmts) < 1 {
		return "", errors.New("Attachments missing for DescribeTasks")
	}
	details := attchmts[0].Details
	if len(details) < 1 {
		return "", errors.New("Details missing for DescribeTasks")
	}
	var ni string
	for _, d := range details {
		if *d.Name == "networkInterfaceId" {
			ni = *d.Value
		}
	}
	return ni, nil

}

// Misc methods

func waitUntilTaskArn(clusterName string, svc *ecs.ECS, waitSecs time.Duration) error {
	listInput := &ecs.ListTasksInput{
		Cluster: aws.String(clusterName),
	}

	timeOutChan := make(chan bool)
	tickChan := time.NewTicker(time.Second * 2).C // DescribeTasks every 5 seconds

	go func() {
		time.Sleep(time.Second * waitSecs)
		timeOutChan <- true
	}()

	for {
		select {
		case <-tickChan:
			d, err := svc.ListTasks(listInput)
			if err != nil {
				return errors.New("ListTasks failed with error: " + err.Error())
			}
			if len(d.TaskArns) > 0 {
				return nil
			}
		case <-timeOutChan:
			return errors.New("Timeout occured for ListTasks Arns for cluster: " + clusterName)
		}
	}

}

func waitUntilTaskActive(clusterName, taskArn string, svc *ecs.ECS, waitSecs time.Duration) error {

	descTaskInput := &ecs.DescribeTasksInput{
		Cluster: aws.String(clusterName),
		Tasks: []*string{
			aws.String(taskArn),
		},
	}
	timeOutChan := make(chan bool)
	tickChan := time.NewTicker(time.Second * 2).C // DescribeTasks every 5 seconds

	go func() {
		time.Sleep(time.Second * waitSecs)
		timeOutChan <- true
	}()

	for {
		select {
		case <-tickChan:
			d, err := svc.DescribeTasks(descTaskInput)
			if err != nil {
				return errors.New("DescribeTasks failed with error: " + err.Error())
			}
			if len(d.Tasks) > 0 && *d.Tasks[0].LastStatus == "RUNNING" {
				return nil
			}
		case <-timeOutChan:
			return errors.New("Timeout occured for DescribeTasks Arns for cluster: " + clusterName)
		}
	}

}

func waitUntilInstanceRunning(instanceID string, svc *ec2.EC2, waitSecs time.Duration) error {

	descStatusInput := &ec2.DescribeInstanceStatusInput{
		InstanceIds: []*string{
			aws.String(instanceID),
		},
	}

	timeOutChan := make(chan bool)
	tickChan := time.NewTicker(time.Second * 2).C // DescribeTasks every 5 seconds

	go func() {
		time.Sleep(time.Second * waitSecs)
		timeOutChan <- true
	}()

	for {
		select {
		case <-tickChan:
			d, err := svc.DescribeInstanceStatus(descStatusInput)
			if err != nil {
				return errors.New("DescribeInstances failed with error: " + err.Error())
			}
			if len(d.InstanceStatuses) > 0 {
				status := d.InstanceStatuses[0]
				if status.InstanceState != nil && *status.InstanceState.Code == 16 {
					return nil
				}
			}
		case <-timeOutChan:
			return errors.New("Timeout occured for DescribeInstances for Instance: " + instanceID)
		}
	}
}

// extractStorage extracts the storage size in GB from the given capacity
// range. If the capacity range is not satisfied it returns the default volume
// size.
func extractStorage(capRange *csi.CapacityRange) (int64, error) {
	if capRange == nil {
		return defaultVolumeSizeInGB, nil
	}

	if capRange.RequiredBytes == 0 && capRange.LimitBytes == 0 {
		return defaultVolumeSizeInGB, nil
	}

	minSize := capRange.RequiredBytes

	// limitBytes might be zero
	maxSize := capRange.LimitBytes
	if capRange.LimitBytes == 0 {
		maxSize = minSize
	}

	if minSize == maxSize {
		return minSize / GB, nil
	}

	return 0, errors.New("requiredBytes and LimitBytes are not the same")
}

// Returns a base64 encoded userData init script
func encodedUserData(volSize int64, devName, masterIP string) string {
	userData := func(volSize, devName, masterIP string) string {
		return `
#!/bin/bash
curl https://gist.githubusercontent.com/maniankara/d4cd6ea36496af6e57b3333c1e882828/raw/fdf07c09f25cd3bf16c56716d95ae5eec4853eb3/provision-moosefs.sh>init.sh
chmod a+x init.sh
./init.sh ` + masterIP + ` ` + volSize + ` ` + devName + `
		`
	}
	userDataStr := userData(strconv.FormatInt(volSize, 10), devName, masterIP)
	return base64.URLEncoding.EncodeToString([]byte(userDataStr))
}

/*
func main() {
	CreateECS(
        "eu-west-1",
		"my_cluster",
}
*/

/*
sess, err := session.NewSession(&aws.Config{Region: aws.String("us-west-2")})
*/

/*
sess, err := session.NewSession(&aws.Config{
    Region:      aws.String("us-west-2"),
    Credentials: credentials.NewSharedCredentials("", "test-account"),
})
*/

/*
svc := ecs.New(session.New())
input := &ecs.CreateClusterInput{
    ClusterName: aws.String("my_cluster"),
}

result, err := svc.CreateCluster(input)
if err != nil {
    if aerr, ok := err.(awserr.Error); ok {
        switch aerr.Code() {
        case ecs.ErrCodeServerException:
            fmt.Println(ecs.ErrCodeServerException, aerr.Error())
        case ecs.ErrCodeClientException:
            fmt.Println(ecs.ErrCodeClientException, aerr.Error())
        case ecs.ErrCodeInvalidParameterException:
            fmt.Println(ecs.ErrCodeInvalidParameterException, aerr.Error())
        default:
            fmt.Println(aerr.Error())
        }
    } else {
        // Print the error, cast err to awserr.Error to get the Code and
        // Message from an error.
        fmt.Println(err.Error())
    }
    return
}

fmt.Println(result)
*/

/*

svc := ec2.New(session.New(&aws.Config{Region: aws.String("us-west-2")}))
    // Specify the details of the instance that you want to create.
    runResult, err := svc.RunInstances(&ec2.RunInstancesInput{
        // An Amazon Linux AMI ID for t2.micro instances in the us-west-2 region
        ImageId:      aws.String("ami-e7527ed7"),
        InstanceType: aws.String("t2.micro"),
        MinCount:     aws.Int64(1),
        MaxCount:     aws.Int64(1),
    })

    if err != nil {
        log.Println("Could not create instance", err)
        return
    }

    log.Println("Created instance", *runResult.Instances[0].InstanceId)

    // Add tags to the created instance
    _ , errtag := svc.CreateTags(&ec2.CreateTagsInput{
        Resources: []*string{runResult.Instances[0].InstanceId},
        Tags: []*ec2.Tag{
            {
                Key:   aws.String("Name"),
                Value: aws.String("MyFirstInstance"),
            },
        },
    })
    if errtag != nil {
        log.Println("Could not create tags for instance", runResult.Instances[0].InstanceId, errtag)
        return
    }

	log.Println("Successfully tagged instance")

*/
