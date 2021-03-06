.PHONY: dispatcher test_game test_client runall rundispatcher rungame runclient killdispatcher killgame killclient killall

all: dispatcher test_game test_client gate

dispatcher:
	cd components/dispatcher && go build

gate:
	cd components/gate && go build

test_game:
	cd examples/test_game && go build

test_client:
	cd examples/test_client && go build

rundispatcher: dispatcher
	components/dispatcher/dispatcher

rungate: gate
	components/gate/gate -gid 1

rungame: test_game
	examples/test_game/test_game -gid=1

restoregame:
	examples/test_game/test_game -gid=1 -log info -restore &
	examples/test_game/test_game -gid=2 -log info -restore &

runclient: test_client
	examples/test_client/test_client -N $(N)

start: dispatcher gate test_game
	components/dispatcher/dispatcher &
	examples/test_game/test_game -gid=1 -log info &
	examples/test_game/test_game -gid=2 -log info &
	components/gate/gate -gid 1 -log info &
	components/gate/gate -gid 2 -log info &

killall:
	-make killclient
	-make killgate
	-make killgame
	-sleep 1
	-make killdispatcher

killgate:
	killall gate

killdispatcher:
	killall dispatcher

killgame:
	killall test_game

freezegame:
	killall -10 test_game

killclient:
	killall test_client
