#!/usr/bin/env bash
#
# Git pre-commit hook that enforces code quality standards
# Run 'make install-hooks' to install this hook
#
set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${YELLOW}Running pre-commit checks...${NC}"

# 1. Format code
echo -e "${YELLOW}[1/2] Running code formatter...${NC}"
if ! make fmt; then
    echo -e "${RED}✗ Code formatting failed${NC}"
    exit 1
fi
echo -e "${GREEN}✓ Code formatted${NC}"

# 2. Run linters
echo -e "${YELLOW}[2/2] Running linters...${NC}"
if ! make lint; then
    echo -e "${RED}✗ Linting failed${NC}"
    echo -e "${YELLOW}Fix the issues above before committing${NC}"
    exit 1
fi
echo -e "${GREEN}✓ Linting passed${NC}"

# Check if formatting made changes that need to be staged
if ! git diff --exit-code > /dev/null 2>&1; then
    echo -e "${YELLOW}Code was formatted. Staging changes...${NC}"
    # Stage all modified files that were originally staged
    git diff --name-only | while read file; do
        if git diff --cached --name-only | grep -q "^$file$"; then
            git add "$file"
        fi
    done
fi

echo -e "${GREEN}✓ All pre-commit checks passed${NC}"
exit 0
