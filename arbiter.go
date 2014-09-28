package main

import (
	"ayu"
	"bufio"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"runtime/pprof"
	"sort"
	"strings"
	"time"
)

type GameState interface {
	Over() bool
	Next() int
	ListMoves() []interface{}
	Execute(arg interface{}) bool
	Scores() (int, int)
	WriteLog(w io.Writer)
}

type Game interface {
	CreateState() GameState
	ParseMove(s string) (interface{}, bool)
}

type AyuGame struct{}

func (ag AyuGame) CreateState() GameState {
	return ayu.CreateState()
}

func (ag AyuGame) ParseMove(s string) (interface{}, bool) {
	return ayu.ParseMove(s)
}

var game AyuGame
var logPath = ""
var msgPath = ""
var cpuprofile = ""
var quiet = false

type Result struct {
	player [2]int     // 0-based player indices
	score  [2]int     // final score
	failed [2]bool    // whether player failed
	points [2]int     // CodeCup-style points
	time   [2]float64 // total time taken
}

type IntPair struct {
	first, second int
}

type IntPairSlice []IntPair

// Functions needed to satisfy sort.Interface:
func (ips IntPairSlice) Len() int {
	return len(ips)
}
func (ips IntPairSlice) Less(i, j int) bool {
	return ips[i].first < ips[j].first ||
		(ips[i].first == ips[j].first && ips[i].second < ips[j].second)
}
func (ips IntPairSlice) Swap(i, j int) {
	ips[i], ips[j] = ips[j], ips[i]
}

func (ips IntPairSlice) Reverse() {
	for i, j := 0, len(ips)-1; i < j; i, j = i+1, j-1 {
		ips.Swap(i, j)
	}
}

func runPlayer(command string, msgPath string) (*exec.Cmd, io.WriteCloser, io.ReadCloser, error) {
	if argv := strings.Fields(command); len(argv) == 0 {
		return nil, nil, nil, os.ErrInvalid
	} else if name, err := exec.LookPath(argv[0]); err != nil {
		return nil, nil, nil, err
	} else if dir, err := os.Getwd(); err != nil {
		return nil, nil, nil, err
	} else {
		cmd := exec.Cmd{Path: name, Args: argv, Dir: dir}
		if stdin, err := cmd.StdinPipe(); err != nil {
			return nil, nil, nil, err
		} else if stdout, err := cmd.StdoutPipe(); err != nil {
			return nil, nil, nil, err
		} else {
			if msgPath == "-" {
				cmd.Stderr = os.Stderr
			} else if msgPath != "" {
				if w, err := os.Create(msgPath); err != nil {
					// Connect to stderr instead
					fmt.Fprintln(os.Stderr, err)
					cmd.Stderr = os.Stderr
				} else {
					cmd.Stderr = w
				}
			}
			if err := cmd.Start(); err != nil {
				return nil, nil, nil, err
			}
			return &cmd, stdin, stdout, nil
		}
	}
}

func runMatch(players [2]int, commands [2]string, logPath string, msgPath [2]string) Result {
	result := Result{player: players}

	var cmds [2]*exec.Cmd
	var readers [2]*bufio.Reader
	var writers [2]io.WriteCloser

	for i := range players {
		if cmd, stdin, stdout, err := runPlayer(commands[i], msgPath[i]); err != nil {
			fmt.Fprintf(os.Stderr, "Couldn't run '%s': %s\n", commands[i], err)
			result.failed[i] = true
		} else {
			cmds[i] = cmd
			writers[i] = stdin
			readers[i] = bufio.NewReader(stdout)
			if i == 0 {
				// Send Start to first player
				fmt.Fprintln(writers[i], "Start")
			}
		}
	}

	var gamestate GameState = game.CreateState()
	over := gamestate.Over()
	for !over {
		moveStr := ""
		p := gamestate.Next()
		if result.failed[p] {
			// Player failed before; move randomly instead:
			moves := gamestate.ListMoves()
			move := moves[rand.Intn(len(moves))]
			if !gamestate.Execute(move) {
				panic("Invalid move generated!")
			}
			moveStr = move.(fmt.Stringer).String()
			over = gamestate.Over()
		} else {
			// Read move from client
			timeStart := time.Now()
			line, err := readers[p].ReadString('\n')
			result.time[p] += float64(time.Now().Sub(timeStart).Nanoseconds()) / 1e9
			if err != nil {
				fmt.Fprintf(os.Stderr, "Failed to read from '%s': %s\n", commands[p], err)
				result.failed[p] = true
			} else {
				line = line[0 : len(line)-1] // discard trailing newline
				if move, ok := game.ParseMove(line); !ok {
					fmt.Fprintf(os.Stderr, "Could not parse move from '%s': %s\n", commands[p], line)
					result.failed[p] = true
				} else if !gamestate.Execute(move) {
					fmt.Fprintf(os.Stderr, "Invalid move from '%s': %s\n", commands[p], line)
					result.failed[p] = true
				} else {
					moveStr = move.(fmt.Stringer).String()
					over = gamestate.Over()
				}
			}
		}
		if moveStr != "" && !result.failed[1-p] && !over {
			if _, err := fmt.Fprintln(writers[1-p], moveStr); err != nil {
				fmt.Fprintf(os.Stderr, "Could not write to '%s': %s\n", commands[1-p], err)
				result.failed[1-p] = true
			}
		}
	}

	// Tell players to quit:
	for _, w := range writers {
		fmt.Fprintln(w, "Quit")
		w.Close()
	}

	// Wait for processes to quit:
	for _, cmd := range cmds {
		cmd.Wait()
	}

	// Determine scores:
	result.score[0], result.score[1] = gamestate.Scores()

	// Determine competition points:
	// FIXME: this should be game-specific too!
	for i := range players {
		if !result.failed[i] {
			result.points[i] = 1
			if result.score[i] > result.score[1-i] {
				result.points[i] += 1
			}
		}
	}

	// Write to log file, if desired:
	if logPath != "" {
		w, err := os.Create(logPath)
		if err != nil {
			fmt.Println(err)
		} else {
			for i := range players {
				fmt.Fprintf(w, "# Player %d: %s\n", i+1, commands[i])
			}
			gamestate.WriteLog(w)
			for i := range players {
				if result.failed[i] {
					fmt.Fprintf(w, "# Player %d failed!\n", i+1)
				}
			}
			summary := fmt.Sprintf("# Score: %d - %d. Time: %.3fs - %.3fs. ",
				result.score[0], result.score[1],
				result.time[0], result.time[1])
			if result.score[0] > result.score[1] {
				summary += "Player 1 won!"
			} else if result.score[1] > result.score[0] {
				summary += "Player 2 won!"
			} else {
				summary += "It's a tie!"
			}
			fmt.Fprintln(w, summary)
			w.Close()
		}
	}

	return result
}

func toYesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func runTournament(commands []string, rounds int, firstOnly bool) []Result {
	if !quiet {
		fmt.Printf(" Id             Player 1                       Player 2             Score   Points  Failed       Time used\n")
		fmt.Printf("---- ------------------------------ ------------------------------  -----  -------  -------  -----------------\n")
	}

	numResults := rounds * len(commands) * (len(commands) - 1)
	if firstOnly {
		numResults = 1
	}
	results := make([]Result, numResults)
	n := 0
outermost:
	for r := 0; r < rounds; r++ {
		for i := range commands {
			for j := range commands {
				if i != j {
					logFilePath := ""
					if logPath != "" {
						logFilePath = fmt.Sprintf("%s%04d.log", logPath, n+1)
					}
					msgFilePath := [2]string{}
					if msgPath != "" {
						if msgPath == "-" {
							msgFilePath[0] = "-"
							msgFilePath[1] = "-"
						} else {
							msgFilePath[0] = fmt.Sprintf("%s%04d.1.log", msgPath, n+1)
							msgFilePath[1] = fmt.Sprintf("%s%04d.2.log", msgPath, n+1)
						}
					}
					res := runMatch([2]int{i, j}, [2]string{commands[i], commands[j]}, logFilePath, msgFilePath)
					player1 := shorten(commands[i], 30)
					player2 := shorten(commands[j], 30)
					if res.score[0] > res.score[1] {
						player1 = strings.ToUpper(player1)
					} else if res.score[1] > res.score[0] {
						player2 = strings.ToUpper(player2)
					}
					if !quiet {
						fmt.Printf(
							"%4d %-30s %-30s  %2d %2d  %3d %3d  %-3s %-3s  %7.3fs %7.3fs\n",
							n+1, player1, player2,
							res.score[0], res.score[1],
							res.points[0], res.points[1],
							toYesNo(res.failed[0]), toYesNo(res.failed[1]),
							res.time[0], res.time[1])
					}
					results[n] = res
					n++
					if firstOnly {
						break outermost
					}
				}
			}
		}
	}
	if !quiet {
		fmt.Printf("---- ------------------------------ ------------------------------  -----  -------  -------  -----------------\n")
	}
	return results
}

func shorten(in string, n int) string {
	if len(in) <= n {
		return in
	}
	if n < 5 {
		return in[0:n]
	}
	a, b := (n-2)/2, (n-2)-(n-2)/2
	return in[0:a] + ".." + in[len(in)-b:]
}

func main() {
	rand.Seed(time.Now().UnixNano())
	rounds := 1
	single := false
	flag.BoolVar(&quiet, "quiet", quiet, "print only plain-text results")
	flag.BoolVar(&single, "single", single, "play only a single game")
	flag.IntVar(&rounds, "rounds", rounds, "number of rounds to play")
	flag.StringVar(&msgPath, "msg", msgPath, "path to player message log files")
	flag.StringVar(&logPath, "log", logPath, "path to game log files")
	flag.StringVar(&cpuprofile, "cpuprofile", cpuprofile, "path to cpu profile")
	flag.Parse()
	if flag.NArg() < 2 {
		fmt.Fprintln(os.Stderr, "Too few player commands passed!")
		fmt.Fprintln(os.Stderr, "Additional options:")
		flag.PrintDefaults()
	} else if rounds < 1 {
		fmt.Fprintln(os.Stderr, "Invalid number of rounds passed!")
	} else if single && (flag.NArg() > 2 || rounds > 1) {
		fmt.Fprintln(os.Stderr, "Single game requires two players and one round!")
	} else {
		if cpuprofile != "" {
			if f, err := os.Create(cpuprofile); err != nil {
				fmt.Println(os.Stderr, "Failed create CPU profile!")
			} else {
				pprof.StartCPUProfile(f)
				defer pprof.StopCPUProfile()
			}
		}
		players := flag.Args()
		results := runTournament(players, rounds, single)
		numGames := rounds * (len(players) - 1) * 2 // per player
		if single {
			numGames = 1
		}

		// Collect some game statistics:
		totalPoints := make([]int, len(players))
		gamesWon := make([]int, len(players))
		gamesTied := make([]int, len(players))
		gamesLost := make([]int, len(players))
		gamesFailed := make([]int, len(players))
		timeUsed := make([]float64, len(players))
		timeMax := make([]float64, len(players))
		winLoss := make([][]int, len(players))
		pairScore := make([][]int, len(players))
		for i := range players {
			winLoss[i] = make([]int, len(players))
			pairScore[i] = make([]int, len(players))
		}
		for _, result := range results {
			for i := 0; i < 2; i++ {
				player := result.player[i]
				opponent := result.player[1-i]
				totalPoints[player] += result.points[i]
				pairScore[player][opponent] += result.score[i]
				if result.failed[i] {
					gamesFailed[player]++
				}
				if result.score[i] > result.score[1-i] {
					gamesWon[player]++
					winLoss[player][result.player[1-i]]++
				}
				if result.score[i] == result.score[1-i] {
					gamesTied[player]++
				}
				if result.score[i] < result.score[1-i] {
					gamesLost[player]++
				}
				timeUsed[player] += result.time[i]
				if result.time[i] > timeMax[player] {
					timeMax[player] = result.time[i]
				}
			}
		}

		if quiet { // Brief results
			for p := range players {
				fmt.Printf("%d\t%d\t%d\t%d\t%d\t%f\t%f\n",
					totalPoints[p], gamesWon[p], gamesTied[p], gamesLost[p],
					gamesFailed[p], timeUsed[p]/float64(numGames), timeMax[p])
			}

		} else { // Verbose results

			// Sort players by total points:
			pointsPlayers := make(IntPairSlice, len(players))
			for i := range pointsPlayers {
				pointsPlayers[i] = IntPair{totalPoints[i], -i}
			}
			sort.Sort(pointsPlayers)
			pointsPlayers.Reverse()

			// Print ranking ordered by Codecup total game points
			fmt.Println()
			fmt.Println("No Player                         Points  Won Tied Lost Fail Avg Time Max Time")
			fmt.Println("-- ------------------------------ ------ ---- ---- ---- ---- -------- --------")
			for i, ip := range pointsPlayers {
				p := -ip.second
				fmt.Printf("%2d %-30s %6d %4d %4d %4d %4d %7.3fs %7.3fs\n",
					i+1, shorten(players[p], 30), totalPoints[p], gamesWon[p], gamesTied[p], gamesLost[p],
					gamesFailed[p], timeUsed[p]/float64(numGames), timeMax[p])
			}
			fmt.Println("-- ------------------------------ ------ ---- ---- ---- ---- -------- --------")

			if len(players) > 2 {
				// Print win/loss matrix
				fmt.Println()
				fmt.Printf("%34s", "")
				for i := range players {
					fmt.Printf(" %2d ", i+1)
				}
				fmt.Println()
				fmt.Printf("%34s", "")
				for _ = range players {
					fmt.Printf(" ---")
				}
				fmt.Println()
				for i, ip := range pointsPlayers {
					p := -ip.second
					fmt.Printf("%2d %30s ", i+1, shorten(players[p], 30))
					for _, jp := range pointsPlayers {
						q := -jp.second
						if p == q {
							fmt.Printf("    ")
						} else {
							fmt.Printf(" %3d", winLoss[p][q])
						}
					}
					fmt.Println()
				}
				fmt.Println("Win count of player 1 (row) against player 2 (column)")
			}

			// Print average difference in points for player against each opponent:
			// NB. Currently DISABLED because this is meaningless for Poly-Y!
			if false && !single {
				fmt.Println()
				fmt.Printf("%34s", "")
				for i := range players {
					fmt.Printf(" %4d  ", i+1)
				}
				fmt.Println()
				fmt.Printf("%34s", "")
				for _ = range players {
					fmt.Printf(" ------")
				}
				fmt.Println()
				for i, ip := range pointsPlayers {
					p := -ip.second
					fmt.Printf("%2d %30s ", i+1, shorten(players[p], 30))
					for _, jp := range pointsPlayers {
						q := -jp.second
						if p == q {
							fmt.Printf("       ")
						} else {
							diff := float64(pairScore[p][q] - pairScore[q][p])
							games := float64(2 * rounds)
							fmt.Printf(" %6.2f", diff/games)
						}
					}
					fmt.Println()
				}
				fmt.Println("Average score difference between players.")
			}
		}
	}
}
