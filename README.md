# Action-House
To run the program, first build the executable:

```bash
go build -o program ./src
```
Then start the nodes with their ports.
The first number is the node’s port, and the following numbers are all the other nodes' ports.
Example:
```bash
./program -node -port 1111 1111 2222
./program -node -port 2222 1111 2222
```
After starting the nodes, run a client:
```bash
./program -id alice 1111 2222
```
From there the client can run:
- bid int
  Places a bid with the given amount

- result
  Returns the highest bid or the final auction result

- end
  Ends the auction immediately
