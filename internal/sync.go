// Copyright (c) 2020, Amazon.com, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package internal ...
package internal

import (
	"context"
	"fmt"
	"io/ioutil"

	"github.com/awslabs/ssosync/internal/aws"
	"github.com/awslabs/ssosync/internal/config"
	"github.com/awslabs/ssosync/internal/google"
	"github.com/hashicorp/go-retryablehttp"

	log "github.com/sirupsen/logrus"
	admin "google.golang.org/api/admin/directory/v1"
)

// SyncGSuite is the interface for synchronising users/groups
type SyncGSuite interface {
	SyncUsers() error
	SyncUsersMatch(string) error
	SyncGroups() error
	SyncGroupsMatch(string) error
	SyncGroupsMatchAndUsers(string) error
}

// SyncGSuite is an object type that will synchronise real users and groups
type syncGSuite struct {
	aws    aws.Client
	google google.Client
	cfg    *config.Config

	users map[string]*aws.User
}

// New will create a new SyncGSuite object
func New(cfg *config.Config, a aws.Client, g google.Client) SyncGSuite {
	return &syncGSuite{
		aws:    a,
		google: g,
		cfg:    cfg,
		users:  make(map[string]*aws.User),
	}
}

// SyncUsers will Sync Google Users to AWS SSO SCIM
func (s *syncGSuite) SyncUsers() error {
	log.Debug("get deleted users")
	deletedUsers, err := s.google.GetDeletedUsers()
	if err != nil {
		log.Warn("Error Getting Deleted Users")
		return err
	}

	for _, u := range deletedUsers {
		log.WithFields(log.Fields{
			"email": u.PrimaryEmail,
		}).Info("deleting google user")

		uu, err := s.aws.FindUserByEmail(u.PrimaryEmail)
		if err != aws.ErrUserNotFound && err != nil {
			log.WithFields(log.Fields{
				"email": u.PrimaryEmail,
			}).Warn("Error deleting google user")
			return err
		}

		if err == aws.ErrUserNotFound {
			log.WithFields(log.Fields{
				"email": u.PrimaryEmail,
			}).Debug("User already deleted")
			continue
		}

		if err := s.aws.DeleteUser(uu); err != nil {
			log.WithFields(log.Fields{
				"email": u.PrimaryEmail,
			}).Warn("Error deleting user")
			return err
		}
	}

	log.Debug("get active google users")
	googleUsers, err := s.google.GetUsers()
	if err != nil {
		return err
	}

	for _, u := range googleUsers {
		if s.ignoreUser(u.PrimaryEmail) {
			continue
		}

		ll := log.WithFields(log.Fields{
			"email": u.PrimaryEmail,
		})

		ll.Debug("finding user")
		uu, _ := s.aws.FindUserByEmail(u.PrimaryEmail)
		if uu != nil {
			s.users[uu.Username] = uu
			// Update the user when suspended state is changed
			if uu.Active == u.Suspended {
				log.Debug("Mismatch active/suspended, updating user")
				// create new user object and update the user
				_, err := s.aws.UpdateUser(aws.UpdateUser(
					uu.ID,
					u.Name.GivenName,
					u.Name.FamilyName,
					u.PrimaryEmail,
					!u.Suspended))
				if err != nil {
					return err
				}
			}
			continue
		}
		ll.Info("creating user ")

		uu, err := s.aws.CreateUser(aws.NewUser(
			u.Name.GivenName,
			u.Name.FamilyName,
			u.PrimaryEmail,
			!u.Suspended))
		if err != nil {
			return err
		}

		s.users[uu.Username] = uu
	}

	return nil
}

// SyncUsersMatch will Sync Google Users to AWS SSO SCIM
// References:
// * https://developers.google.com/admin-sdk/directory/v1/guides/search-users
// query possible values:
//  name:'Jane'
//  email:admin*
//  isAdmin=true
//  manager='janesmith@example.com'
//  orgName=Engineering orgTitle:Manager
//  EmploymentData.projects:'GeneGnomes'
func (s *syncGSuite) SyncUsersMatch(query string) error {

	log.Debug("get active google users")
	googleUsers, err := s.google.GetUsersMatch(query)
	if err != nil {
		return err
	}

	for _, u := range googleUsers {
		if s.ignoreUser(u.PrimaryEmail) {
			continue
		}

		ll := log.WithFields(log.Fields{
			"email": u.PrimaryEmail,
		})

		ll.Debug("finding user")
		uu, _ := s.aws.FindUserByEmail(u.PrimaryEmail)
		if uu != nil {
			s.users[uu.Username] = uu
			// Update the user when suspended state is changed
			if uu.Active == u.Suspended {
				log.Debug("Mismatch active/suspended, updating user")
				// create new user object and update the user
				_, err := s.aws.UpdateUser(aws.UpdateUser(
					uu.ID,
					u.Name.GivenName,
					u.Name.FamilyName,
					u.PrimaryEmail,
					!u.Suspended))
				if err != nil {
					return err
				}
			}
			continue
		}

		ll.Info("creating user ")
		uu, err := s.aws.CreateUser(aws.NewUser(
			u.Name.GivenName,
			u.Name.FamilyName,
			u.PrimaryEmail,
			!u.Suspended))
		if err != nil {
			return err
		}

		s.users[uu.Username] = uu
	}

	return nil
}

// SyncGroups will sync groups from Google -> AWS SSO
func (s *syncGSuite) SyncGroups() error {
	log.Debug("get google groups")
	googleGroups, err := s.google.GetGroups()
	if err != nil {
		return err
	}

	correlatedGroups := make(map[string]*aws.Group)

	for _, g := range googleGroups {
		if s.ignoreGroup(g.Email) {
			continue
		}

		log := log.WithFields(log.Fields{
			"group": g.Email,
		})

		log.Debug("Check group")

		var group *aws.Group

		gg, err := s.aws.FindGroupByDisplayName(g.Email)
		if err != nil && err != aws.ErrGroupNotFound {
			return err
		}

		if gg != nil {
			log.Debug("Found group")
			correlatedGroups[gg.DisplayName] = gg
			group = gg
		} else {
			log.Info("Creating group in AWS")
			newGroup, err := s.aws.CreateGroup(aws.NewGroup(g.Email))
			if err != nil {
				return err
			}
			correlatedGroups[newGroup.DisplayName] = newGroup
			group = newGroup
		}

		groupMembers, err := s.google.GetGroupMembers(g)
		if err != nil {
			return err
		}

		memberList := make(map[string]*admin.Member)

		log.Info("Start group user sync")

		for _, m := range groupMembers {
			if _, ok := s.users[m.Email]; ok {
				memberList[m.Email] = m
			}
		}

		for _, u := range s.users {
			log.WithField("user", u.Username).Debug("Checking user is in group already")
			b, err := s.aws.IsUserInGroup(u, group)
			if err != nil {
				return err
			}

			if _, ok := memberList[u.Username]; ok {
				if !b {
					log.WithField("user", u.Username).Info("Adding user to group")
					err := s.aws.AddUserToGroup(u, group)
					if err != nil {
						return err
					}
				}
			} else {
				if b {
					log.WithField("user", u.Username).Info("Removing user from group")
					err := s.aws.RemoveUserFromGroup(u, group)
					if err != nil {
						return err
					}
				}
			}
		}
	}

	return nil
}

// SyncGroupsMatch will sync groups from Google -> AWS SSO
// References:
// * https://developers.google.com/admin-sdk/directory/v1/guides/search-groups
// query possible values:
//  name='contact'
//  email:admin*
//  memberKey=user@company.com
//  name:contact* email:contact*
//  name:Admin* email:aws-*
//  email:aws-*
func (s *syncGSuite) SyncGroupsMatch(query string) error {

	log.WithField("query", query).Debug("get google groups")
	googleGroups, err := s.google.GetGroupsMatch(query)
	if err != nil {
		return err
	}

	correlatedGroups := make(map[string]*aws.Group)

	for _, g := range googleGroups {
		if s.ignoreGroup(g.Email) {
			continue
		}

		log := log.WithFields(log.Fields{
			"group": g.Email,
		})

		log.Debug("Check group")
		var group *aws.Group

		gg, err := s.aws.FindGroupByDisplayName(g.Email)
		if err != nil && err != aws.ErrGroupNotFound {
			return err
		}

		if gg != nil {
			log.Debug("Found group")
			correlatedGroups[gg.DisplayName] = gg
			group = gg
		} else {
			log.Info("Creating group in AWS")
			newGroup, err := s.aws.CreateGroup(aws.NewGroup(g.Email))
			if err != nil {
				return err
			}
			correlatedGroups[newGroup.DisplayName] = newGroup
			group = newGroup
		}

		groupMembers, err := s.google.GetGroupMembers(g)
		if err != nil {
			return err
		}

		memberList := make(map[string]*admin.Member)

		log.Info("Start group user sync")

		for _, m := range groupMembers {
			if _, ok := s.users[m.Email]; ok {
				memberList[m.Email] = m
			}
		}

		for _, u := range s.users {
			log.WithField("user", u.Username).Debug("Checking user is in group already")
			b, err := s.aws.IsUserInGroup(u, group)
			if err != nil {
				return err
			}

			if _, ok := memberList[u.Username]; ok {
				if !b {
					log.WithField("user", u.Username).Info("Adding user to group")
					err := s.aws.AddUserToGroup(u, group)
					if err != nil {
						return err
					}
				}
			} else {
				if b {
					log.WithField("user", u.Username).Info("Removing user from group")
					err := s.aws.RemoveUserFromGroup(u, group)
					if err != nil {
						return err
					}
				}
			}
		}
	}

	return nil
}

// SyncGroupsMatchAndUsers will sync groups from Google -> AWS SSO
// References:
// * https://developers.google.com/admin-sdk/directory/v1/guides/search-groups
// query possible values:
//  name='contact'
//  email:admin*
//  memberKey=user@company.com
//  name:contact* email:contact*
//  name:Admin* email:aws-*
//  email:aws-*
// process worflow:
//  1) delete users in aws, these were deleted in google
//  2) update users in aws, these were updated in google
//  3) add users in aws, these were added in google
//  4) add groups in aws and add its members, these were added in google
//  5) validate equals aws an google groups members
//  6) delete groups in aws, these were deleted in google
func (s *syncGSuite) SyncGroupsMatchAndUsers(query string) error {

	log.WithField("query", query).Debug("get google groups")
	googleGroups, err := s.google.GetGroupsMatch(query)
	if err != nil {
		return err
	}

	log.Debug("get google users and groups members")
	googleUsers := make([]*admin.User, 0)
	googleGroupsUsers := make(map[string][]*admin.User, len(googleGroups))
	for _, g := range googleGroups {

		log := log.WithFields(log.Fields{
			"group": g.Name,
		})

		log.Debug("get group members")

		groupMembers, err := s.google.GetGroupMembers(g)
		if err != nil {
			return err
		}

		log.Info("get user")
		for _, m := range groupMembers {
			q := fmt.Sprintf("email:%s", m.Email)
			u, err := s.google.GetUsersMatch(q) // TODO: implemnet GetUser(q)
			if err != nil {
				return err
			}
			googleUsers = append(googleUsers, u[0])
		}
		googleGroupsUsers[g.Name] = googleUsers
	}

	log.Debug("get aws groups")
	awsGroups, err := s.aws.GetGroups()
	if err != nil {
		return err
	}

	log.Debug("get aws users")
	awsUsers, err := s.aws.GetUsers()
	if err != nil {
		return err
	}

	// list of changes
	addAWSUsers, delAWSUsers, updateAWSUsers, _ := getUserOperations(awsUsers, googleUsers)
	addAWSGroups, delAWSGroups, equalAWSGroups := getGroupOperations(awsGroups, googleGroups)

	// delete aws users (deleted in google)
	for _, awsUser := range delAWSUsers {

		log := log.WithFields(log.Fields{"user": awsUser.Username})

		log.Warn("deleting user")

		if err := s.aws.DeleteUser(awsUser); err != nil {
			log.Error("error deleting user")
			return err
		}
	}

	// update aws users (updated in google)
	for _, awsUser := range updateAWSUsers {

		log := log.WithFields(log.Fields{"user": awsUser.Username})

		log.Debug("updating user")

		_, err := s.aws.UpdateUser(awsUser)
		if err != nil {
			log.Error("error updating user")
			return err
		}
	}

	// add aws users (new in google)
	for _, awsUser := range addAWSUsers {

		log := log.WithFields(log.Fields{"user": awsUser.Username})

		log.Debug("creating user")

		_, err := s.aws.CreateUser(awsUser)
		if err != nil {
			log.Error("error creating user")
			return err
		}
	}

	// add aws groups (new in google)
	for _, awsGroup := range addAWSGroups {

		log := log.WithFields(log.Fields{"group": awsGroup.DisplayName})

		log.Debug("creating group")

		_, err := s.aws.CreateGroup(awsGroup)
		if err != nil {
			log.Error("creating group")
			return err
		}

		// add members of the new group
		for _, googleUser := range googleGroupsUsers[awsGroup.DisplayName] {

			// equivalent aws user of google user on the fly
			awsUser := aws.NewUser(
				googleUser.Name.GivenName,
				googleUser.Name.FamilyName,
				googleUser.PrimaryEmail,
				!googleUser.Suspended)

			log.WithField("user", awsUser.Username).Info("adding user to group")
			err := s.aws.AddUserToGroup(awsUser, awsGroup)
			if err != nil {
				return err
			}
		}
	}

	// validate equals aws an google groups members
	for _, awsGroup := range equalAWSGroups {

		// add members of the new group
		log := log.WithFields(log.Fields{"group": awsGroup.DisplayName})

		for _, googleUser := range googleGroupsUsers[awsGroup.DisplayName] {

			// equivalent aws user of google user on the fly
			awsUser := aws.NewUser(
				googleUser.Name.GivenName,
				googleUser.Name.FamilyName,
				googleUser.PrimaryEmail,
				!googleUser.Suspended)

			log.WithField("user", awsUser.Username).Debug("Checking user is in group already")
			b, err := s.aws.IsUserInGroup(awsUser, awsGroup)
			if err != nil {
				return err
			}

			if !b {
				log.WithField("user", awsUser.Username).Info("adding user to group")
				err := s.aws.AddUserToGroup(awsUser, awsGroup)
				if err != nil {
					return err
				}
			}
		}
	}

	// delete aws groups (deleted in google)
	for _, awsGroup := range delAWSGroups {

		log := log.WithFields(log.Fields{"group": awsGroup.DisplayName})

		log.Warn("deleting group")

		err := s.aws.DeleteGroup(awsGroup)
		if err != nil {
			log.Error("deleting group")
			return err
		}
	}

	return nil
}

// getGroupOperations returns the groups of AWS that must be added, deleted and are equals
func getGroupOperations(awsGroups []*aws.Group, googleGroups []*admin.Group) (add []*aws.Group, delete []*aws.Group, equals []*aws.Group) {

	awsMap := make(map[string]*aws.Group, len(awsGroups))
	googleMap := make(map[string]struct{}, len(googleGroups))

	for _, awsGroup := range awsGroups {
		awsMap[awsGroup.DisplayName] = awsGroup
	}

	for _, gGroup := range googleGroups {
		googleMap[gGroup.Name] = struct{}{}
	}

	// AWS Groups found and not found in google
	for _, gGroup := range googleGroups {
		if _, found := awsMap[gGroup.Name]; found {
			equals = append(equals, awsMap[gGroup.Name])
		} else {
			add = append(add, aws.NewGroup(gGroup.Name))
		}
	}

	// Google Groups founds and not in aws
	for _, awsGroup := range awsGroups {
		if _, found := googleMap[awsGroup.DisplayName]; !found {
			delete = append(delete, aws.NewGroup(awsGroup.DisplayName))
		}
	}

	return add, delete, equals
}

// getUserOperations returns the users of AWS that must be added, deleted, updated and are equals
func getUserOperations(awsUsers []*aws.User, googleUsers []*admin.User) (add []*aws.User, delete []*aws.User, update []*aws.User, equals []*aws.User) {

	awsMap := make(map[string]*aws.User, len(awsUsers))
	googleMap := make(map[string]struct{}, len(googleUsers))

	for _, awsUser := range awsUsers {
		awsMap[awsUser.Username] = awsUser
	}

	for _, gUser := range googleUsers {
		googleMap[gUser.PrimaryEmail] = struct{}{}
	}

	// AWS Users found and not found in google
	for _, gUser := range googleUsers {
		if awsUser, found := awsMap[gUser.PrimaryEmail]; found {
			if awsUser.Active == gUser.Suspended ||
				awsUser.Name.GivenName != gUser.Name.GivenName ||
				awsUser.Name.FamilyName != gUser.Name.FamilyName {
				update = append(update, aws.NewUser(gUser.Name.GivenName, gUser.Name.FamilyName, gUser.PrimaryEmail, !gUser.Suspended))
			} else {
				equals = append(equals, awsUser)
			}
		} else {
			add = append(add, aws.NewUser(gUser.Name.GivenName, gUser.Name.FamilyName, gUser.PrimaryEmail, !gUser.Suspended))
		}
	}

	// Google Users founds and not in aws
	for _, awsUser := range awsUsers {
		if _, found := googleMap[awsUser.Username]; !found {
			delete = append(delete, aws.NewUser(awsUser.Name.GivenName, awsUser.Name.FamilyName, awsUser.Username, awsUser.Active))
		}
	}

	return add, delete, update, equals
}

// DoSync will create a logger and run the sync with the paths
// given to do the sync.
func DoSync(ctx context.Context, cfg *config.Config) error {
	log.Info("Syncing AWS users and groups from Google Workspace SAML Application")

	creds := []byte(cfg.GoogleCredentials)

	if !cfg.IsLambda {
		b, err := ioutil.ReadFile(cfg.GoogleCredentials)
		if err != nil {
			return err
		}
		creds = b
	}

	// create a http client with retry and backoff capabilities
	retryClient := retryablehttp.NewClient()
	retryClient.Logger = log.StandardLogger()
	httpClient := retryClient.StandardClient()

	googleClient, err := google.NewClient(ctx, cfg.GoogleAdmin, creds)
	if err != nil {
		return err
	}

	awsClient, err := aws.NewClient(
		httpClient,
		&aws.Config{
			Endpoint: cfg.SCIMEndpoint,
			Token:    cfg.SCIMAccessToken,
		})
	if err != nil {
		return err
	}

	c := New(cfg, awsClient, googleClient)
	//err = c.SyncUsers()
	err = c.SyncUsersMatch(cfg.UserMatch)
	if err != nil {
		return err
	}

	//err = c.SyncGroups()
	err = c.SyncGroupsMatch(cfg.GroupMatch)
	if err != nil {
		return err
	}

	return nil
}

func (s *syncGSuite) ignoreUser(name string) bool {
	for _, u := range s.cfg.IgnoreUsers {
		if u == name {
			return true
		}
	}

	return false
}

func (s *syncGSuite) ignoreGroup(name string) bool {
	for _, g := range s.cfg.IgnoreGroups {
		if g == name {
			return true
		}
	}

	return false
}
