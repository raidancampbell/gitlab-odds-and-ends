package main

import (
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	"github.com/xanzy/go-gitlab"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"os"
)

const (
	GITLAB_TOKEN_ENV_VAR      = "GITLAB_TOKEN"
	MR_STATE_OPENED           = "opened"
	MR_STATE_CLOSED           = "closed"
	MR_STATE_LOCKED           = "locked"
	MR_STATE_MERGED           = "merged"
	HEADER_GITLAB_EVENT       = "X-Gitlab-Event"
	HEADER_TYPE_MERGE_REQUEST = "Merge Request Hook"
)

func GitlabWrapper(gitlab *gitlab.Client, f func(gitlab *gitlab.Client, c *gin.Context)) func(c *gin.Context) {
	return func(c *gin.Context) {
		f(gitlab, c)
	}
}

func main() {
	git, err := gitlab.NewClient(os.Getenv(GITLAB_TOKEN_ENV_VAR), gitlab.WithBaseURL("http://nuc.sinkhole.raidancampbell.com:2080/api/v4"))
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}

	r := gin.Default()
	r.POST("/gitlab/callback", GitlabWrapper(git, gitlabCallbackRouter))

	listenaddr := ":8080"
	logrus.Info("listening on " + listenaddr)
	panic(r.Run(listenaddr))
}

func gitlabCallbackRouter(gitlab *gitlab.Client, c *gin.Context) {
	switch c.Request.Header.Get(HEADER_GITLAB_EVENT) {
	case HEADER_TYPE_MERGE_REQUEST:
		mergeRequest(gitlab, c)
	default:
		logrus.Errorf("Not handling event '%s', because we don't care about it", c.Request.Header.Get(HEADER_GITLAB_EVENT))
		http.Error(c.Writer, http.StatusText(http.StatusNoContent), http.StatusNoContent)
		return
	}
}

// mergeRequest receives an MR,
func mergeRequest(gl *gitlab.Client, c *gin.Context) {
	mr := MRCallback{}
	b, err := ioutil.ReadAll(c.Request.Body)
	if err != nil {
		logrus.WithError(err).Error("failed to read MR callback")
		http.Error(c.Writer, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		return
	}
	err = json.Unmarshal(b, &mr)
	if err != nil {
		logrus.WithError(err).Error("failed to unmarshal MR callback")
		http.Error(c.Writer, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		return
	}

	c.Writer.WriteHeader(http.StatusOK)

	logrus.Debugf("processing merge request webhook %+v", mr)

	var repo, author, assignee, url string
	var isWIP bool

	url = mr.ObjectAttributes.URL
	repo = mr.ObjectAttributes.Target.Name
	isWIP = mr.ObjectAttributes.WorkInProgress

	authorID := mr.ObjectAttributes.AuthorID
	user, _, err := gl.Users.GetUser(authorID)
	if err != nil {
		logrus.WithError(err).Errorf("failed to get author of MR %s, with user ID %d", url, authorID)
		http.Error(c.Writer, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		return
	}

	author = user.Name

	// TODO: what are the valid states?
	// action: update, approved, close, unapproved, reopen, open
	// TODO: if it's a newly created MR, assign a maintainer to it

	switch mr.ObjectAttributes.Action {
	case "open":
	case "update":
	case "approved":
	case "merge":
	case "unapproved":
	case "close":
	case "reopen":
	}

	err = assignMaintainer(err, gl, mr)
	if err != nil {
		logrus.WithError(err).Error("Failed to assign maintainer to merge request")
		return
	}

	var msg string
	if isWIP {
		msg = fmt.Sprintf("New WIP merge request in %s from %s has been assigned to %s.  See %s for details.", repo, author, assignee, url)
	} else {
		msg = fmt.Sprintf("New merge request in %s from %s has been assigned to %s.  See %s for details.", repo, author, assignee, url)
	}

	logrus.Info(msg)

	// TODO: send notification
}

func assignMaintainer(err error, gl *gitlab.Client, mr MRCallback) error {
	maintainers, err := getProjectMaintainers(gl, mr.Project.ID)
	if err != nil {
		return err
	}
	if len(maintainers) == 0 {
		return fmt.Errorf("no maintainers for repository, cannot assign a maintainer")
	}
	if mr.ObjectAttributes.AssigneeID == 0 {
		_, _, err = gl.MergeRequests.UpdateMergeRequest(mr.Project.ID, mr.ObjectAttributes.Iid, &gitlab.UpdateMergeRequestOptions{
			AssigneeID: &maintainers[rand.Intn(len(maintainers))].ID,
		})
	} else {
		found := false
		for _, maintainer := range maintainers {
			if maintainer.ID == mr.ObjectAttributes.AssigneeID {
				found = true
				break
			}
		}
		if !found {
			// someone must have assigned it to themselves, or the maintainer was hit by a bus.  reassign
			_, _, err = gl.MergeRequests.UpdateMergeRequest(mr.Project.ID, mr.ObjectAttributes.Iid, &gitlab.UpdateMergeRequestOptions{
				AssigneeID: &maintainers[rand.Intn(len(maintainers))].ID,
			})
		}
	}
	return err
}

func getProjectMaintainers(gl *gitlab.Client, id int) (maintainers []*gitlab.ProjectMember, err error) {
	// not inherited.  if you want inherited, slap on a `/all` at the end

	page := 0
	members, _, err := gl.ProjectMembers.ListProjectMembers(id, &gitlab.ListProjectMembersOptions{
		ListOptions: gitlab.ListOptions{
			Page:    page,
			PerPage: 100,
		},
		Query: nil,
	})
	for ; ; {
		if err != nil {
			break
		}
		for _, m := range members {
			if m == nil {
				break
			}
			if m.AccessLevel >= gitlab.MaintainerPermissions {
				maintainers = append(maintainers, m)
			}
		}
		if len(members) < 100 {
			break
		}

		page++
		members, _, err = gl.ProjectMembers.ListProjectMembers(id, &gitlab.ListProjectMembersOptions{
			ListOptions: gitlab.ListOptions{
				Page:    page,
				PerPage: 100,
			},
			Query: nil,
		})
	}

	return maintainers, err

}
