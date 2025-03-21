#!/usr/bin/env bash
set -euo pipefail

## 10-100
SPEED=30
#SPEED=1000

function cmd() {
    local cmd="${1}"
    if [[ ! -z ${cmd} ]]; then
        echo ""    
        echo "> ${cmd}" | pv -qL "${SPEED}"
        read -n 1 -s
        echo ""
        if [[ ! ${cmd} =~ ^## ]]; then
            eval $cmd
        fi
        # echo -e "\n"
    fi
}

function prompt() {
    local cmd="${1}"
    if [[ ! -z ${cmd} ]]; then
        echo "> ${cmd}" | pv -qL "${SPEED}"
        read -n 1 -s
        if [[ ! ${cmd} =~ ^## ]]; then
            eval $cmd
        fi
        # echo -e "\n"
    fi
}