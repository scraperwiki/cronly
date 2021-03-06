package main

// Why!?
// Vixie cron has some troubles under our absurd usage of it at ScraperWiki.
// It seemed like a good idea to schedule all of our users' tasks using cron.
// In addition, it seemed like a good idea to clobber the crontab from version
// control. Every minute. Using cron.

// Cron is usually the highest contributor to load on our system, so I thought
// take a crack at it.

// Cronly does a lot less checking than vixie-cron. Less stat'ing, checking users
// exist, etc etc. This should result in a fairly large win, since vixie stats all
// crontabs and compares things with the passwd database frequently.
// Cronly essentially reads files only when they change, and never looks in the
// user database.

// Notes:

// * We can continue to use vixie for system jobs. We still use crontab.
// * Cronly not a suitable replacement for system cron (doesn't do /cron.d/ etc.)
// * We can test this in parallel with the existing infrastructure
//     (with -dry-run, comparing logs)
// * Actual number of jobs per minute from free.scraperwiki.com: ~50
// * Number of hours cobalt-f running for at time of measurement: 103.75
// * Number of CPU-hours consumed by cron at time of measurement: 102
// * Total jobs: 1,827,880/month -- ~2,500/hour

// * Numbers quoted here seem absurd but are useful when thinking about amount of
//   CPU time consumed over time.

// * Measurements are all made unthrottled, i.e, running jobs as fast as we can.
// * CPU time consumed by jobly algorithms for a month of free jobs: 2.7s (== wall time)
// * Cronly can manage ~.6M jobs/sec under good conditions (single CPU)
// * Receives instant notification of exact crontabs changing via inotify
//   (doesn't need to readdir on /var/spool/cron/crontabs directory)
// * Efficient algorithm for crontab updates (most operations ln(total N jobs))
// * Speed dip barely noticable even if every file in crontab directory is being
//   sequentially touched as fast as possible
// * Under extreme conditions, memory usage stable at ~25MiB (cf vixie @ 17MiB)
// * If every job causes a reload of its crontab, speed is more like ~21k jobs/sec
//   (purely algorithmic, no `touch`)
// * Even creating the exec.Command() structure slows things down considerably
//     to more like ~70k jobs/sec (down from .5M)
// * Invoking bash takes us down to ~380/sec, with CPU usage of jobly at ~30%.
//   jobly time for 1 day of jobs invoking bash every time (60,937 jobs):
//	   wall 2m45s user 37s sys 2m23
//   (exec'ing is expensive!)

// Based on someone else's cron expression parser:
// https://github.com/gorhill/cronexpr

// Cronly makes it obvious that there are a few invalid crons
// (that contain HTML 504 gateway timeouts) in some cases

// Security model:
// * Cronly assumes that the crontabs directory is only writable by root and makes
//     no effort to check.
// * Not yet implemented: cronly doesn't take any responsibility for the execution
//     environment. That's for the execution wrapper shell script to wrangle.

// Not yet implemented:

// * TODO(pwaller): Actually invoking the job
// * Ideas:
//   * Maybe we could exec a shell script which can do the hard work
//	   * Checking the user is valid
//     * Su'ing
//     * Mailing
//   * For a given minute, we could attempt to spread jobs out throughout that minute.
//     (however we might also just hand them off to `jobly` ;-)
//   * Not thought about time jumps. :( No idea what happens when clock changes.

// Time jumps, what I think happens currently:
// Forwards: things get run really fast until we catch up
// Backwards: we don't re-run things, we sleep for a long time.
// What should happen for time jumps?
// Could take some inspiration from vixie.

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/bitly/go-nsq"
	"github.com/howeyc/fsnotify"
)

var (
	dryRun       = flag.Bool("dry-run", false, "Run a dry run")
	fast         = flag.Bool("fast", false, "Run as fast as possible")
	medium       = flag.Bool("medium", false, "Slightly slower than fast: sleep every 10 millisecond")
	crontabsPath = flag.String("crontabs", "/var/spool/cron/crontabs", "crontab directory")
	verbose      = flag.Bool("verbose", false, "Increase verbosity")
)

func main() {

	flag.Parse()

	// Wait for interrupt
	done := make(chan struct{})
	go func() {
		c := make(chan os.Signal)
		signal.Notify(c, os.Interrupt)
		<-c
		close(done)
		done = nil
	}()

	// Watch for new crontabs
	newCrontab := make(chan string, 100)
	go func() {
		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			panic(err)
		}

		go func() {
			for event := range watcher.Event {
				if event.Name == *crontabsPath {
					// Don't follow the directory itself
					continue
				}
				if strings.HasPrefix(event.Name, filepath.Join(*crontabsPath, "tmp.")) {
					// Don't watch tmp. files
					continue
				}
				newCrontab <- event.Name
			}
		}()

		err = watcher.Watch(*crontabsPath)
		if err != nil {
			panic(err)
		}
		// Should never reach here if we have functioning Watch().
	}()

	jobsInvoked := 0
	defer func() {
		log.Println("Total jobs invoked:", jobsInvoked)
	}()

	fastChan := make(chan struct{})
	if *fast {
		close(fastChan)
	}

	// Remove leading ./ so that fsnotify paths match whatever goes
	// into the update dictionary.
	*crontabsPath = filepath.Clean(*crontabsPath)

	// Read cron
	crontabs := ReadCrontabs(*crontabsPath)
	if *verbose {
		log.Println("Have", len(crontabs), "crontabs")
	}

	queue := *NewJobsFromCrontabs(crontabs)

	nsqConfig := nsq.NewConfig()
	nsqProducer, err := nsq.NewProducer("localhost:nsq", nsqConfig)
	if err != nil {
		log.Fatalln("Unable to connect to nsq")
	}

	hostname, err := os.Hostname()
	if err != nil {
		log.Fatalln("Unable to determine hostname:", err)
	}
	topic := fmt.Sprintf("cron.%s", hostname)

	// finish := time.Now().Add(1 * 24 * time.Hour)

	var newCrontabs int

	// Main loop
	for done != nil { // && runTime.Before(finish) {
		var runTime time.Time
		if queue.Len() > 0 {
			runTime = queue.Top().nextRun
		} else {
			// If there is nothing in the schedule, wait 500ms.
			runTime = time.Now().Add(500 * time.Millisecond)
		}

		wait := -time.Since(runTime)

		if *medium {
			// "medium" is slightly slower than "fast"
			wait = 10 * time.Millisecond
		}
		// log.Println("Next run in", wait, "@", runTime)

		select {
		case <-done:
			// Signalled to quit
			return

		case crontab := <-newCrontab:
			// log.Println("New crontab:", crontab)
			newCrontabs++

			// UpdateCrontab needs to know the epoch for new jobs.
			// We use `after` to ensure that new jobs just appearing will have
			// their next runtime in the future (and in particular, have the
			// opportunity to next run before the current top of queue).
			after := time.Now()
			if *fast {
				// When fast we have to fiddle it (because we're not using wall clock
				// time); we use the time when we would next consider running
				// something (so that they can pop up after the next batch).
				after = runTime
			}
			queue.UpdateCrontab(after, crontab)
			continue

		case <-fastChan:
			// This path is like an optional `default` case, allowing
			// us to short-circuit all the other cases

		case <-time.After(wait):
		}

		// Pull from the top of the queue
		// and reschedule the pulled jobs
		jobsThisIteration := queue.NextJobs()

		if *verbose {
			if *medium || *fast {
				log.Printf("At %v invoking %v jobs (%d new crontabs)", runTime, jobsThisIteration.Len(), newCrontabs)
			} else {
				log.Printf("Invoking %v jobs (%d new crontabs)", jobsThisIteration.Len(), newCrontabs)
			}
		}

		if !*dryRun {
			jobsThisIteration.Invoke(&queue)
			message, err := jobsThisIteration.Marshal()
			if err != nil {
				log.Println("Marshal Error:", err)
				continue
			}

			err = nsqProducer.MultiPublish(topic, message)
			if err != nil {
				log.Println("Publish Error:", err)
				continue
			}
		}

		jobsInvoked += jobsThisIteration.Len()
		newCrontabs = 0
	}
}
