package core

import (
	"github.com/J-Leg/player-count/src/db"
	"github.com/J-Leg/player-count/src/env"
	"github.com/J-Leg/player-count/src/stats"
	"github.com/cheggaaa/pb/v3"
	"math"
	"os"
	"time"
)

// Constants
const (
	MONTHS   = 12
	DURATION = 8
)

// Execute : Core execution for daily updates
// Update all apps
func Execute(cfg *env.Config) {
	var dbcfg db.Dbcfg = db.Dbcfg(*cfg)
	cfg.Trace.Debug.Printf("initiate daily execution.")

	appList, err := dbcfg.GetAppList()
	if err != nil {
		return
	}

	bar := pb.StartNew(len(appList))
	bar.SetRefreshRate(time.Second)
	bar.SetWriter(os.Stdout)
	bar.Start()

	for _, app := range appList {

		// Update progress
		bar.Increment()
		time.Sleep(time.Millisecond)
		err := processApp(&dbcfg, &app)
		if err != nil {
			continue
		}
	}
	bar.Finish()
	cfg.Trace.Debug.Printf("conclude daily execution.")

	return
}

// ExecuteMonthly : Monthly process
func ExecuteMonthly(cfg *env.Config) {
	var dbcfg db.Dbcfg = db.Dbcfg(*cfg)
	cfg.Trace.Debug.Printf("initiate monthly execution.")

	appList, err := dbcfg.GetAppList()
	if err != nil {
		cfg.Trace.Error.Printf("failed to retrieve app list. %s", err)
		return
	}

	currentDateTime := time.Now()
	var monthToAvg time.Month = currentDateTime.Month() - 1
	if currentDateTime.Month() == 1 {
		monthToAvg = 12
	}

	var yearToAvg int = currentDateTime.Year()
	if currentDateTime.Month() == 1 {
		yearToAvg = currentDateTime.Year() - 1
	}

	workChannel := make(chan bool)

	for _, app := range appList {
		go processAppMonthly(&dbcfg, app, monthToAvg, yearToAvg, workChannel)
	}

	// Progress
	bar := pb.StartNew(len(appList))
	bar.SetRefreshRate(time.Second)
	bar.SetWriter(os.Stdout)
	bar.Start()

	var numSuccess, numErrors int = 0, 0
	defer finalise(&numSuccess, &numErrors, workChannel, bar, cfg)

	timeout := time.After(DURATION * time.Minute)

	for i := 0; i < len(appList); i++ {
		bar.Increment()
		select {
		case msg := <-workChannel:
			if msg {
				numSuccess++
			} else {
				numErrors++
			}
		case <-timeout:
			cfg.Trace.Info.Printf("Monthly process runtime exceeding maximum duration.")
			return
		}
	}
	return
}

func finalise(numSuccess, numError *int, ch chan<- bool, bar *pb.ProgressBar, cfg *env.Config) {
	close(ch)
	bar.Finish()
	cfg.Trace.Info.Printf("conclude monthly execution.\nREPORT: \n    success: %d\n"+"    errors: %d", *numSuccess, *numError)
}

// ExecuteRecovery : Best effort to retry all exception instances
func ExecuteRecovery(cfg *env.Config) {
	var dbcfg db.Dbcfg = db.Dbcfg(*cfg)
	var appsToUpdate, err = dbcfg.GetExceptions()
	if err != nil {
		cfg.Trace.Error.Printf("Error retrieving exceptions. %s", err)
		return
	}

	dbcfg.FlushExceptions()

	cfg.Trace.Info.Printf("[Exceptions] re-do daily process.")
	for _, app := range *appsToUpdate {
		err = processApp(&dbcfg, &app)
		if err != nil {
			cfg.Trace.Error.Printf("Daily retry (%s) failed for app: %+v - %s", app.Date, app.Ref.ID, err)
			continue
		}
	}

	cfg.Trace.Info.Printf("[Exceptions] recovery process complete.")

	return
}

func processAppMonthly(
	cfg *db.Dbcfg,
	app db.AppShadow,
	monthToAvg time.Month,
	yearToAvg int,
	ch chan<- bool) {

	cfg.Trace.Debug.Printf("monthly process on app: %s - ID: %+v.", app.Ref.Name, app.Ref.ID)

	var err error
	defer workDone(ch, &err)

	dailyMetricList, err := cfg.GetDailyList(app.Ref.ID)
	if err != nil {
		cfg.Trace.Error.Printf("Error retrieving daily metric: %s", err)
		return
	}
	// Initialise a new list
	var newDailyMetricList []stats.DailyMetric

	var total int = 0
	var numCounted int = 0
	var newPeak float64 = 0

	for _, dailyMetric := range *dailyMetricList {
		var elementMonth = dailyMetric.Date.Month()
		var elementYear = dailyMetric.Date.Year()
		var monthDiff = monthToAvg - elementMonth

		// Only keep daily metrics up to the last 3 months
		// This condition "should" be enough if older months were correctly purged
		if (monthDiff+MONTHS)%MONTHS < 3 {

			// Secondary conidition is just for assurance
			if monthDiff < 0 && (elementYear != yearToAvg-1) {
				continue
			}

			newDailyMetricList = append(newDailyMetricList, dailyMetric)
		}

		if elementMonth != monthToAvg {
			continue
		}

		newPeak = math.Max(newPeak, float64(dailyMetric.PlayerCount))
		total += dailyMetric.PlayerCount
		numCounted++
	}

	var newAverage int = 0
	if numCounted > 0 {
		newAverage = total / numCounted
	}

	cfg.Trace.Debug.Printf("Computed average player count of: %d on month: %d using %d dates.",
		newAverage, monthToAvg, numCounted)

	err = cfg.UpdateDailyList(app.Ref.ID, &newDailyMetricList)
	if err != nil {
		cfg.Trace.Error.Printf("Error updating daily metric list: %s.", err)
		return
	}

	var monthMetricListPtr *[]db.Metric
	monthMetricListPtr, err = cfg.GetMonthlyList(app.Ref.ID)
	if err != nil {
		cfg.Trace.Error.Printf("Error retrieving month metrics: %s.", err)
		return
	}

	monthSort(monthMetricListPtr)

	monthMetricList := *monthMetricListPtr
	previousMonthMetrics := &monthMetricList[len(monthMetricList)-1]

	cfg.Trace.Info.Printf("Construct month element: month - %s, year - %d", monthToAvg.String(), yearToAvg)

	newMonth := constructNewMonthMetric(previousMonthMetrics, newPeak, float64(newAverage), monthToAvg, yearToAvg)
	monthMetricList = append(monthMetricList, *newMonth)

	cfg.UpdateMonthlyList(app.Ref.ID, &monthMetricList)
	if err != nil {
		cfg.Trace.Error.Printf("error updating month metric list %s.", err)
		return
	}

	cfg.Trace.Debug.Printf("monthly process success for app: %s - ID: %+v.", app.Ref.Name, app.Ref.ID)
	return
}

func processApp(cfg *db.Dbcfg, app *db.AppShadow) error {
	cfg.Trace.Debug.Printf("daily process on app: %s - id: %+v.", app.Ref.Name, app.Ref.ID)
	dm, err := stats.Fetch(app.Date, app.Ref.Domain, app.Ref.DomainID)
	if err != nil {
		err = cfg.PushException(app)
		if err != nil {
			cfg.Trace.Error.Printf("error inserting app %d to exception queue! %s", app.Ref.DomainID, err)
			// What do?
		}
		return err
	}

	err = cfg.PushDaily(app.Ref.ID, dm)
	if err != nil {
		err = cfg.PushException(app)
		if err != nil {
			cfg.Trace.Error.Printf("error inserting app %d to exception queue! %s", app.Ref.DomainID, err)
			// What do?
		}
		return err
	}
	cfg.Trace.Debug.Printf("daily process success on app: %s - id: %+v.", app.Ref.Name, app.Ref.ID)
	return nil
}

func workDone(ch chan<- bool, err *error) {
	if *err == nil {
		ch <- true
	} else {
		ch <- false
	}
}
