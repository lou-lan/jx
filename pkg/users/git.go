package users

import (
	"fmt"

	"gopkg.in/src-d/go-git.v4/plumbing/object"

	jenkinsv1 "github.com/jenkins-x/jx/pkg/apis/jenkins.io/v1"
	"github.com/jenkins-x/jx/pkg/log"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	jenkninsv1client "github.com/jenkins-x/jx/pkg/client/clientset/versioned"

	"github.com/jenkins-x/jx/pkg/gits"
)

// GitUserResolver allows git users to be converted to Jenkins X users
type GitUserResolver struct {
	GitProvider gits.GitProvider
	JXClient    jenkninsv1client.Interface
	Namespace   string
}

// GitSignatureAsUser resolves the signature to a Jenkins X User
func (r *GitUserResolver) GitSignatureAsUser(signature *object.Signature) (*jenkinsv1.User, error) {
	gitUser := &gits.GitUser{
		Email: signature.Email,
		Name:  signature.Name,
	}
	return r.GitUserAsUser(gitUser)
}

// GitUserSliceAsUserDetailsSlice resolves a slice of git users to a slice of Jenkins X User Details
func (r *GitUserResolver) GitUserSliceAsUserDetailsSlice(users []gits.GitUser) ([]jenkinsv1.UserDetails, error) {
	answer := []jenkinsv1.UserDetails{}
	for _, user := range users {
		u, err := r.GitUserAsUser(&user)
		if err != nil {
			return nil, err
		}
		answer = append(answer, u.Spec)
	}
	return answer, nil
}

// GitUserAsUser will convert the GitUser to a Jenkins X user and attempt to complete the user info by:
// * checking the user custom resources to see if the user is present there
// * making a call to the gitProvider
// as often user info is not complete in a git response
func (r *GitUserResolver) GitUserAsUser(user *gits.GitUser) (*jenkinsv1.User, error) {
	if user == nil {
		return nil, fmt.Errorf("user cannot be nil")
	}
	providerKey := r.GitProviderKey()
	if user.Login != "" {

		labelSelector := fmt.Sprintf("%s=%s", providerKey, user.Login)

		// First try to find by label - this is much faster as it uses an index
		users, err := r.JXClient.JenkinsV1().Users(r.Namespace).List(metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			return nil, err
		}
		if len(users.Items) > 1 {
			return nil, fmt.Errorf("more than one user found in users.jenkins.io with label %s, found %v", labelSelector,
				users.Items)
		} else if len(users.Items) == 1 {
			return &users.Items[0], nil
		}
	}

	// Next try without the label - this might occur if someone manually updated the list
	users, err := r.JXClient.JenkinsV1().Users(r.Namespace).List(metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	if user.Login != "" {

		possibles := make([]jenkinsv1.User, 0)
		for _, u := range users.Items {
			for _, a := range u.Spec.Accounts {
				if a.Provider == providerKey && a.ID == user.Login {
					possibles = append(possibles, u)
				}
			}
		}
		if len(possibles) > 1 {
			possibleUsers := make([]string, 0)
			for _, p := range possibles {
				possibleUsers = append(possibleUsers, p.Name)
			}
			return nil, fmt.Errorf("more than one user found in users.jenkins.io with login %s for GitProvider %s, "+
				"found %s", user.Login, providerKey, possibleUsers)
		} else if len(possibles) == 1 {
			// Add the label for next time
			found := &possibles[0]
			if found.Labels == nil {
				found.Labels = make(map[string]string)
			}
			found.Labels[providerKey] = user.Login
			found, err := r.JXClient.JenkinsV1().Users(r.Namespace).Update(found)
			if err != nil {
				return nil, err
			}
			log.Infof("Adding label %s=%s to user %s in users.jenkins.io\n", providerKey, user.Login, found.Name)
			return found, nil
		}
	}

	// Finally, try to resolve by email address against git user, by performing a remote git call
	gitUser := r.GitProvider.UserInfo(user.Login)
	possibles := make([]jenkinsv1.User, 0)
	if gitUser == nil {
		// annoyingly UserInfo swallows the error, so we recreate it!
		log.Warnf("unable to find user with login %s from %s", user.Login, r.GitProvider.Kind())
	} else {
		for _, u := range users.Items {
			if u.Spec.Email == gitUser.Email {
				possibles = append(possibles, u)
			}
		}
	}
	if len(possibles) > 1 {
		possibleStrings := make([]string, 0)
		for _, p := range possibles {
			possibleStrings = append(possibleStrings, p.Name)
		}
		return nil, fmt.Errorf("more than one user found in users.jenkins.io with email %s, found %v",
			gitUser.Email, possibleStrings)
	} else if len(possibles) == 1 {
		found := &possibles[0]
		// Add the git id to the user
		if found.Spec.Accounts == nil {
			found.Spec.Accounts = make([]jenkinsv1.AccountReference, 0)
		}
		found.Spec.Accounts = append(found.Spec.Accounts, jenkinsv1.AccountReference{
			ID:       gitUser.Login,
			Provider: providerKey,
		})
		// Add the label as well
		if found.Labels == nil {
			found.Labels = make(map[string]string)
		}
		found.Labels[providerKey] = user.Login
		found, err := r.JXClient.JenkinsV1().Users(r.Namespace).Update(found)
		log.Infof("Associating user %s in users.jenkins.io with email %s to git GitProvider user with login %s as "+
			"emails match\n", found.Name, found.Spec.Email, gitUser.Login)
		log.Infof("Adding label %s=%s to user %s in users.jenkins.io\n", providerKey, user.Login, found.Name)
		if err != nil {
			return nil, err
		}
		return found, nil
	} else {
		// Otherwise, create a new user using the best info we have
		// gitUser (looked up using the git provider API) is the default,
		// but let's see if anything from user we were passed in as an argument can help
		u := r.GitUserToUser(mergeGitUsers(gitUser, user))
		return r.JXClient.JenkinsV1().Users(r.Namespace).Create(u)
	}
	return nil, nil
}

// UpdateUserFromPRAuthor will attempt to use the
func (r *GitUserResolver) UpdateUserFromPRAuthor(author *jenkinsv1.User, pullRequest *gits.GitPullRequest,
	commits []*gits.GitCommit) (*jenkinsv1.User, error) {

	if pullRequest != nil {
		updated := false
		if author != nil {
			gitLogin := r.GitUserLogin(author)
			if gitLogin == "" {
				gitLogin = author.Spec.Login
			}
			for _, commit := range commits {
				if commit.Author != nil && gitLogin == commit.Author.Login {
					log.Info("Found commit author match for: " + author.
						Spec.Login + " with email address: " + commit.Author.Email + "\n")
					author.Spec.Email = commit.Author.Email
					updated = true
					break
				}
			}
		}
		if updated {
			return r.JXClient.JenkinsV1().Users(r.Namespace).Update(author)
		}
	}
	return author, nil
}

// UserToGitUser performs type conversion from a Jenkins X User to a Git User
func (r *GitUserResolver) UserToGitUser(id string, user *jenkinsv1.User) *gits.GitUser {
	return &gits.GitUser{
		Login:     id,
		Email:     user.Spec.Email,
		Name:      user.Spec.Name,
		URL:       user.Spec.URL,
		AvatarURL: user.Spec.AvatarURL,
	}
}

// GitUserToUser performs type conversion from a GitUser to a Jenkins X user,
// attaching the Git Provider account to Accounts
func (r *GitUserResolver) GitUserToUser(gitUser *gits.GitUser) *jenkinsv1.User {
	user := CreateUser(r.Namespace, gitUser.Login, gitUser.Name, gitUser.Email)
	return AddAccountReference(user, r.GitProviderKey(), gitUser.Login)
}

// GitUserLogin returns the login for the git provider, or an empty string if not found
func (r *GitUserResolver) GitUserLogin(user *jenkinsv1.User) string {
	for _, a := range user.Spec.Accounts {
		if a.Provider == r.GitProviderKey() {
			return a.ID
		}
	}
	return ""
}

// GitProviderKey returns the provider key for this GitUserResolver
func (r *GitUserResolver) GitProviderKey() string {
	return fmt.Sprintf("jenkins.io/git-%s-userid", r.GitProvider.Kind())
}

// mergeGitUsers merges user1 into user2, replacing any that do not have empty values on user2 with those from user1
func mergeGitUsers(user1 *gits.GitUser, user2 *gits.GitUser) *gits.GitUser {
	if user1 == nil {
		return user2
	}
	if user2 == nil {
		return user1
	}
	answer := &gits.GitUser{}
	if user1.AvatarURL != "" {
		answer.AvatarURL = user1.AvatarURL
	} else {
		answer.AvatarURL = user2.AvatarURL
	}
	if user1.URL != "" {
		answer.URL = user1.URL
	} else {
		answer.URL = user2.URL
	}
	if user1.Name != "" {
		answer.Name = user1.Name
	} else {
		answer.Name = user2.Name
	}
	if user1.Login != "" {
		answer.Login = user1.Login
	} else {
		answer.Login = user2.Login
	}
	if user1.Email != "" {
		answer.Email = user1.Email
	} else {
		answer.Email = user2.Email
	}
	return answer
}
