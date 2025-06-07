#!/bin/bash

./startup.sh
read -p "Press Enter to continue..."
./change.sh
read -p "Press Enter to continue..."
./cleanup.sh