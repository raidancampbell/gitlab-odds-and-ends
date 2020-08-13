package main

import (
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	"github.com/slack-go/slack"
	"github.com/xanzy/go-gitlab"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"os"
)

const (
	SLACK_TOKEN_ENV_VAR  = "SLACK_TOKEN"
	GITLAB_TOKEN_ENV_VAR = "GITLAB_TOKEN"
	MR_ACTION_OPENED     = "open"
	MR_ACTION_UPDATED    = "update"
	MR_ACTION_APPROVED   = "approved"
	MR_ACTION_MERGED     = "merge"
	MR_ACTION_UNAPPROVED = "unapproved"
	MR_ACTION_CLOSED     = "close"
	MR_ACTION_REOPENED   = "reopen"
	HEADER_GITLAB_EVENT  = "X-Gitlab-Event"
)

// gitlab project ID to slack channel map
// anytime you register this webhook to another repo,
// a mapping should be added here
var repoSlackChannelMapping = map[int]string{
	1: "C1234567890",
	2: "C1234567890",
	3: "C1234567890",
}

func HandlerWrapper(gitlab *gitlab.Client, rtm *slack.RTM, f func(gitlab *gitlab.Client, rtm *slack.RTM, c *gin.Context)) func(c *gin.Context) {
	return func(c *gin.Context) {
		f(gitlab, rtm, c)
	}
}

func main() {
	git, err := gitlab.NewClient(os.Getenv(GITLAB_TOKEN_ENV_VAR), gitlab.WithBaseURL("http://nuc.sinkhole.raidancampbell.com:2080/api/v4"))
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}

	var rtm = new(slack.RTM)
	if os.Getenv(SLACK_TOKEN_ENV_VAR) != "" {
		slk := slack.New(os.Getenv(SLACK_TOKEN_ENV_VAR))

		rtm = slk.NewRTM()
		go rtm.ManageConnection()
	} else {
		// TODO: build a noop copy of RTM, and wrap RTM in an interface
		logrus.Warn("no slack token set, slack messaging disabled")
	}

	r := gin.Default()
	r.POST("/gitlab/callback", HandlerWrapper(git, rtm, gitlabCallbackRouter))

	listenaddr := ":8080"
	logrus.Info("listening on " + listenaddr)
	panic(r.Run(listenaddr))
}

func gitlabCallbackRouter(gl *gitlab.Client, rtm *slack.RTM, c *gin.Context) {
	b, err := ioutil.ReadAll(c.Request.Body)
	if err != nil {
		logrus.Errorf("Failed to read request body '%w'", err)
		http.Error(c.Writer, http.StatusText(http.StatusNoContent), http.StatusNoContent)
	}

	webhook, err := gitlab.ParseWebhook(gitlab.WebhookEventType(c.Request), b)
	if err != nil {
		logrus.Errorf("Failed to parse gitlab webhook with type '%s', '%w'", c.Request.Header.Get(HEADER_GITLAB_EVENT), err)
		http.Error(c.Writer, http.StatusText(http.StatusNoContent), http.StatusNoContent)
	}

	switch wh := webhook.(type) {
	case *gitlab.MergeEvent: // actually a Merge Request event...
		c.Writer.WriteHeader(http.StatusOK)
		mergeRequest(gl, rtm, wh)
	default:
		logrus.Errorf("Not handling event '%s', because we don't care about it", c.Request.Header.Get(HEADER_GITLAB_EVENT))
		http.Error(c.Writer, http.StatusText(http.StatusNoContent), http.StatusNoContent)
	}
}

// mergeRequest receives an MR
func mergeRequest(gl *gitlab.Client, rtm *slack.RTM, mr *gitlab.MergeEvent) {

	logrus.SetLevel(logrus.DebugLevel)

	logrus.Debugf("processing merge request webhook %+v", mr)

	// TODO: what are the valid states? this docs page is not accurate for MR callbacks: https://docs.gitlab.com/ce/api/events.html#action-types

	switch mr.ObjectAttributes.Action {
	case MR_ACTION_OPENED:
		// assign
		assignee, err := maybeAssignMaintainer(gl, mr)
		if err != nil {
			logrus.WithError(err).Error("Failed to assign maintainer to merge request")
			return
		}

		// notify
		notifyNewMR(mr, err, gl, assignee, rtm)

		// TODO: save notification thread ID for any updates
	case MR_ACTION_UPDATED:
		// check if approved
		// if approved,
	case MR_ACTION_APPROVED:
	case MR_ACTION_MERGED:
	case MR_ACTION_UNAPPROVED:
	case MR_ACTION_CLOSED:
	case MR_ACTION_REOPENED:
	}

}

func notifyNewMR(mr *gitlab.MergeEvent, err error, gl *gitlab.Client, assignee string, rtm *slack.RTM) {
	user, _, err := gl.Users.GetUser(mr.ObjectAttributes.AuthorID)
	if err != nil {
		logrus.WithError(err).Error("unable to see who opened the merge request. continuing...")
	}
	author := user.Name

	url := mr.ObjectAttributes.URL
	repo := mr.ObjectAttributes.Target.Name
	isWIP := mr.ObjectAttributes.WorkInProgress

	var msg string
	if isWIP {
		msg = fmt.Sprintf("New WIP merge request in %s from %s has been assigned to %s.  See %s for details.", repo, author, assignee, url)
	} else {
		msg = fmt.Sprintf("New merge request in %s from %s has been assigned to %s.  See %s for details.", repo, author, assignee, url)
	}
	logrus.Info(msg)

	targetChannel, ok := repoSlackChannelMapping[mr.Project.ID]
	if !ok {
		logrus.Warnf("No slack channel configured for project ID %d", mr.Project.ID)
	} else if rtm != nil {
		rtm.SendMessage(rtm.NewOutgoingMessage(msg, targetChannel))
	}
}

// maybeAssignMaintainer will ensure the given MR has a maintainer assigned to it
// if no maintainer is assigned, a maintainer/owner from the target repository is chosen at random and assigned
// if someone is assigned and is not a maintainer (i.e. the requester self-assigned),
// then it is reassigned to a random maintainer.  If an existing maintainer is already assigned, they remain in place.
// Returns the maintainer's Name, and any errors encountered
func maybeAssignMaintainer(gl *gitlab.Client, mr *gitlab.MergeEvent) (string, error) {
	maintainers, err := getProjectMaintainers(gl, mr.Project.ID)
	if err != nil {
		return "", err
	}
	if len(maintainers) == 0 {
		return "", fmt.Errorf("no maintainers for repository, cannot assign a maintainer")
	}
	maintainer := maintainers[rand.Intn(len(maintainers))]

	if mr.ObjectAttributes.AssigneeID == 0 {
		_, _, err = gl.MergeRequests.UpdateMergeRequest(mr.Project.ID, mr.ObjectAttributes.IID, &gitlab.UpdateMergeRequestOptions{
			AssigneeID: &maintainer.ID,
		})
		return maintainer.Name, err
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
			_, _, err = gl.MergeRequests.UpdateMergeRequest(mr.Project.ID, mr.ObjectAttributes.IID, &gitlab.UpdateMergeRequestOptions{
				AssigneeID: &maintainer.ID,
			})
			return maintainer.Name, err
		}
	}
	//TODO: this is blank
	return mr.ObjectAttributes.Assignee.Name, err
}

// getProjectMaintainers lists the maintainers of the given project.  This does not include inherited permissions.
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
